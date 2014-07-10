// Package pgx is a PostgreSQL database driver.
//
// It does not implement the standard database/sql interface.
package pgx

import (
	"bufio"
	"crypto/md5"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	log "gopkg.in/inconshreveable/log15.v2"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Transaction isolation levels
const (
	Serializable    = "serializable"
	RepeatableRead  = "repeatable read"
	ReadCommitted   = "read committed"
	ReadUncommitted = "read uncommitted"
)

// ConnConfig contains all the options used to establish a connection.
type ConnConfig struct {
	Host      string // host (e.g. localhost) or path to unix domain socket directory (e.g. /private/tmp)
	Port      uint16 // default: 5432
	Database  string
	User      string // default: OS user name
	Password  string
	TLSConfig *tls.Config // config for TLS connection -- nil disables TLS
	Logger    log.Logger
}

// Conn is a PostgreSQL connection handle. It is not safe for concurrent usage.
// Use ConnPool to manage access to multiple database connections from multiple
// goroutines.
type Conn struct {
	conn               net.Conn      // the underlying TCP or unix domain socket connection
	reader             *bufio.Reader // buffered reader to improve read performance
	wbuf               [1024]byte
	Pid                int32             // backend pid
	SecretKey          int32             // key to use to send a cancel query message to the server
	RuntimeParams      map[string]string // parameters that have been reported by the server
	config             ConnConfig        // config used when establishing this connection
	TxStatus           byte
	preparedStatements map[string]*PreparedStatement
	notifications      []*Notification
	alive              bool
	causeOfDeath       error
	logger             log.Logger
	qr                 QueryResult
	mr                 MsgReader
}

type PreparedStatement struct {
	Name              string
	FieldDescriptions []FieldDescription
	ParameterOids     []Oid
}

type Notification struct {
	Pid     int32  // backend pid that sent the notification
	Channel string // channel from which notification was received
	Payload string
}

type CommandTag string

// RowsAffected returns the number of rows affected. If the CommandTag was not
// for a row affecting command (such as "CREATE TABLE") then it returns 0
func (ct CommandTag) RowsAffected() int64 {
	words := strings.Split(string(ct), " ")
	n, _ := strconv.ParseInt(words[len(words)-1], 10, 64)
	return n
}

// NotSingleRowError is returned when exactly 1 row is expected, but 0 or more than
// 1 row is returned
type NotSingleRowError struct {
	RowCount int64
}

func (e NotSingleRowError) Error() string {
	return fmt.Sprintf("Expected to find 1 row exactly, instead found %d", e.RowCount)
}

type ProtocolError string

func (e ProtocolError) Error() string {
	return string(e)
}

var NotificationTimeoutError = errors.New("Notification Timeout")
var DeadConnError = errors.New("Connection is dead")

// Connect establishes a connection with a PostgreSQL server using config.
// config.Host must be specified. config.User will default to the OS user name.
// Other config fields are optional.
func Connect(config ConnConfig) (c *Conn, err error) {
	c = new(Conn)

	c.config = config
	if c.config.Logger != nil {
		c.logger = c.config.Logger
	} else {
		c.logger = log.New()
		c.logger.SetHandler(log.DiscardHandler())
	}

	if c.config.User == "" {
		user, err := user.Current()
		if err != nil {
			return nil, err
		}
		c.config.User = user.Username
		c.logger.Debug("Using default connection config", "User", c.config.User)
	}

	if c.config.Port == 0 {
		c.config.Port = 5432
		c.logger.Debug("Using default connection config", "Port", c.config.Port)
	}

	// See if host is a valid path, if yes connect with a socket
	_, err = os.Stat(c.config.Host)
	if err == nil {
		// For backward compatibility accept socket file paths -- but directories are now preferred
		socket := c.config.Host
		if !strings.Contains(socket, "/.s.PGSQL.") {
			socket = filepath.Join(socket, ".s.PGSQL.") + strconv.FormatInt(int64(c.config.Port), 10)
		}

		c.logger.Info(fmt.Sprintf("Dialing PostgreSQL server at socket: %s", socket))
		c.conn, err = net.Dial("unix", socket)
		if err != nil {
			c.logger.Error(fmt.Sprintf("Connection failed: %v", err))
			return nil, err
		}
	} else {
		c.logger.Info(fmt.Sprintf("Dialing PostgreSQL server at host: %s:%d", c.config.Host, c.config.Port))
		c.conn, err = net.Dial("tcp", fmt.Sprintf("%s:%d", c.config.Host, c.config.Port))
		if err != nil {
			c.logger.Error(fmt.Sprintf("Connection failed: %v", err))
			return nil, err
		}
	}
	defer func() {
		if c != nil && err != nil {
			c.conn.Close()
			c.alive = false
			c.logger.Error(err.Error())
		}
	}()

	c.RuntimeParams = make(map[string]string)
	c.preparedStatements = make(map[string]*PreparedStatement)
	c.alive = true

	if config.TLSConfig != nil {
		c.logger.Debug("Starting TLS handshake")
		if err = c.startTLS(); err != nil {
			c.logger.Error(fmt.Sprintf("TLS failed: %v", err))
			return
		}
	}

	c.reader = bufio.NewReader(c.conn)
	c.mr.reader = c.reader

	msg := newStartupMessage()
	msg.options["user"] = c.config.User
	if c.config.Database != "" {
		msg.options["database"] = c.config.Database
	}
	if err = c.txStartupMessage(msg); err != nil {
		return
	}

	for {
		var t byte
		var r *MsgReader
		t, r, err = c.rxMsg()
		if err != nil {
			return nil, err
		}

		switch t {
		case backendKeyData:
			c.rxBackendKeyData(r)
		case authenticationX:
			if err = c.rxAuthenticationX(r); err != nil {
				return nil, err
			}
		case readyForQuery:
			c.rxReadyForQuery(r)
			c.logger = c.logger.New("pid", c.Pid)
			c.logger.Info("Connection established")
			return c, nil
		default:
			if err = c.processContextFreeMsg(t, r); err != nil {
				return nil, err
			}
		}
	}
}

// Close closes a connection. It is safe to call Close on a already closed
// connection.
func (c *Conn) Close() (err error) {
	if !c.IsAlive() {
		return nil
	}

	wbuf := newWriteBuf(c.wbuf[0:0], 'X')
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)

	c.die(errors.New("Closed"))
	c.logger.Info("Closed connection")
	return err
}

// ParseURI parses a database URI into ConnConfig
func ParseURI(uri string) (ConnConfig, error) {
	var cp ConnConfig

	url, err := url.Parse(uri)
	if err != nil {
		return cp, err
	}

	if url.User != nil {
		cp.User = url.User.Username()
		cp.Password, _ = url.User.Password()
	}

	parts := strings.SplitN(url.Host, ":", 2)
	cp.Host = parts[0]
	if len(parts) == 2 {
		p, err := strconv.ParseUint(parts[1], 10, 16)
		if err != nil {
			return cp, err
		}
		cp.Port = uint16(p)
	}
	cp.Database = strings.TrimLeft(url.Path, "/")

	return cp, nil
}

// Prepare creates a prepared statement with name and sql. sql can contain placeholders
// for bound parameters. These placeholders are referenced positional as $1, $2, etc.
func (c *Conn) Prepare(name, sql string) (ps *PreparedStatement, err error) {
	defer func() {
		if err != nil {
			c.logger.Error(fmt.Sprintf("Prepare `%s` as `%s` failed: %v", name, sql, err))
		}
	}()

	// parse
	wbuf := newWriteBuf(c.wbuf[0:0], 'P')
	wbuf.WriteCString(name)
	wbuf.WriteCString(sql)
	wbuf.WriteInt16(0)

	// describe
	wbuf.startMsg('D')
	wbuf.WriteByte('S')
	wbuf.WriteCString(name)

	// sync
	wbuf.startMsg('S')
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)
	if err != nil {
		return nil, err
	}

	ps = &PreparedStatement{Name: name}

	var softErr error

	for {
		var t byte
		var r *MsgReader
		t, r, err := c.rxMsg()
		if err != nil {
			return nil, err
		}

		switch t {
		case parseComplete:
		case parameterDescription:
			ps.ParameterOids = c.rxParameterDescription(r)
		case rowDescription:
			ps.FieldDescriptions = c.rxRowDescription(r)
			for i := range ps.FieldDescriptions {
				switch ps.FieldDescriptions[i].DataType {
				case BoolOid, ByteaOid, Int2Oid, Int4Oid, Int8Oid, Float4Oid, Float8Oid, DateOid, TimestampTzOid:
					ps.FieldDescriptions[i].FormatCode = BinaryFormatCode
				}
			}
		case noData:
		case readyForQuery:
			c.rxReadyForQuery(r)
			c.preparedStatements[name] = ps
			return ps, softErr
		default:
			if e := c.processContextFreeMsg(t, r); e != nil && softErr == nil {
				softErr = e
			}
		}
	}
}

// Deallocate released a prepared statement
func (c *Conn) Deallocate(name string) (err error) {
	delete(c.preparedStatements, name)
	_, err = c.Exec("deallocate " + QuoteIdentifier(name))
	return
}

// Listen establishes a PostgreSQL listen/notify to channel
func (c *Conn) Listen(channel string) (err error) {
	_, err = c.Exec("listen " + channel)
	return
}

// WaitForNotification waits for a PostgreSQL notification for up to timeout.
// If the timeout occurs it returns pgx.NotificationTimeoutError
func (c *Conn) WaitForNotification(timeout time.Duration) (*Notification, error) {
	if len(c.notifications) > 0 {
		notification := c.notifications[0]
		c.notifications = c.notifications[1:]
		return notification, nil
	}

	var zeroTime time.Time
	stopTime := time.Now().Add(timeout)

	for {
		// Use SetReadDeadline to implement the timeout. SetReadDeadline will
		// cause operations to fail with a *net.OpError that has a Timeout()
		// of true. Because the normal pgx rxMsg path considers any error to
		// have potentially corrupted the state of the connection, it dies
		// on any errors. So to avoid timeout errors in rxMsg we set the
		// deadline and peek into the reader. If a timeout error occurs there
		// we don't break the pgx connection. If the Peek returns that data
		// is available then we turn off the read deadline before the rxMsg.
		err := c.conn.SetReadDeadline(stopTime)
		if err != nil {
			return nil, err
		}

		// Wait until there is a byte available before continuing onto the normal msg reading path
		_, err = c.reader.Peek(1)
		if err != nil {
			c.conn.SetReadDeadline(zeroTime) // we can only return one error and we already have one -- so ignore possiple error from SetReadDeadline
			if err, ok := err.(*net.OpError); ok && err.Timeout() {
				return nil, NotificationTimeoutError
			}
			return nil, err
		}

		err = c.conn.SetReadDeadline(zeroTime)
		if err != nil {
			return nil, err
		}

		var t byte
		var r *MsgReader
		if t, r, err = c.rxMsg(); err == nil {
			if err = c.processContextFreeMsg(t, r); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}

		if len(c.notifications) > 0 {
			notification := c.notifications[0]
			c.notifications = c.notifications[1:]
			return notification, nil
		}
	}
}

func (c *Conn) IsAlive() bool {
	return c.alive
}

func (c *Conn) CauseOfDeath() error {
	return c.causeOfDeath
}

type Row QueryResult

func (r *Row) Scan(dest ...interface{}) (err error) {
	qr := (*QueryResult)(r)

	if qr.Err() != nil {
		return qr.Err()
	}

	if !qr.NextRow() {
		if qr.Err() == nil {
			return errors.New("No rows")
		} else {
			return qr.Err()
		}
	}

	qr.Scan(dest...)
	qr.Close()
	return qr.Err()
}

type QueryResult struct {
	pool      *ConnPool
	conn      *Conn
	mr        *MsgReader
	fields    []FieldDescription
	rowCount  int
	columnIdx int
	err       error
	closed    bool
}

func (qr *QueryResult) FieldDescriptions() []FieldDescription {
	return qr.fields
}

func (qr *QueryResult) MsgReader() *MsgReader {
	return qr.mr
}

func (qr *QueryResult) close() {
	if qr.pool != nil {
		qr.pool.Release(qr.conn)
		qr.pool = nil
	}

	qr.closed = true
}

func (qr *QueryResult) readUntilReadyForQuery() {
	for {
		t, r, err := qr.conn.rxMsg()
		if err != nil {
			qr.close()
			return
		}

		switch t {
		case readyForQuery:
			qr.conn.rxReadyForQuery(r)
			qr.close()
			return
		case rowDescription:
		case dataRow:
		case commandComplete:
		case bindComplete:
		default:
			err = qr.conn.processContextFreeMsg(t, r)
			if err != nil {
				qr.close()
				return
			}
		}
	}
}

func (qr *QueryResult) Close() {
	if qr.closed {
		return
	}
	qr.readUntilReadyForQuery()
	qr.close()
}

func (qr *QueryResult) Err() error {
	return qr.err
}

func (qr *QueryResult) Fatal(err error) {
	qr.err = err
	qr.Close()
}

func (qr *QueryResult) NextRow() bool {
	if qr.closed {
		return false
	}

	qr.rowCount++
	qr.columnIdx = 0

	for {
		t, r, err := qr.conn.rxMsg()
		if err != nil {
			qr.Fatal(err)
			return false
		}

		switch t {
		case readyForQuery:
			qr.conn.rxReadyForQuery(r)
			qr.close()
			return false
		case dataRow:
			fieldCount := r.ReadInt16()
			if int(fieldCount) != len(qr.fields) {
				qr.Fatal(ProtocolError(fmt.Sprintf("Row description field count (%v) and data row field count (%v) do not match", len(qr.fields), fieldCount)))
				return false
			}

			qr.mr = r
			return true
		case commandComplete:
		case bindComplete:
		default:
			err = qr.conn.processContextFreeMsg(t, r)
			if err != nil {
				qr.Fatal(err)
				return false
			}
		}
	}
}

func (qr *QueryResult) nextColumn() (*FieldDescription, int32, bool) {
	if qr.closed {
		return nil, 0, false
	}
	if len(qr.fields) <= qr.columnIdx {
		qr.Fatal(ProtocolError("No next column available"))
		return nil, 0, false
	}

	fd := &qr.fields[qr.columnIdx]
	qr.columnIdx++
	size := qr.mr.ReadInt32()
	return fd, size, true
}

func (qr *QueryResult) Scan(dest ...interface{}) (err error) {
	if len(qr.fields) != len(dest) {
		err = errors.New("Scan received wrong number of arguments")
		qr.Fatal(err)
		return err
	}

	for _, d := range dest {
		fd, size, _ := qr.nextColumn()
		switch d := d.(type) {
		case *bool:
			*d = decodeBool(qr, fd, size)
		case *[]byte:
			*d = decodeBytea(qr, fd, size)
		case *int64:
			*d = decodeInt8(qr, fd, size)
		case *int16:
			*d = decodeInt2(qr, fd, size)
		case *int32:
			*d = decodeInt4(qr, fd, size)
		case *string:
			*d = decodeText(qr, fd, size)
		case *float32:
			*d = decodeFloat4(qr, fd, size)
		case *float64:
			*d = decodeFloat8(qr, fd, size)
		case *time.Time:
			if fd.DataType == DateOid {
				*d = decodeDate(qr, fd, size)
			} else {
				*d = decodeTimestampTz(qr, fd, size)
			}

		case Scanner:
			err = d.Scan(qr, fd, size)
			if err != nil {
				return err
			}
		default:
			return errors.New("Unknown type")
		}
	}

	return nil
}

func (qr *QueryResult) ReadValue() (v interface{}, err error) {
	fd, size, _ := qr.nextColumn()
	if qr.Err() != nil {
		return nil, qr.Err()
	}

	switch fd.DataType {
	case BoolOid:
		return decodeBool(qr, fd, size), qr.Err()
	case ByteaOid:
		return decodeBytea(qr, fd, size), qr.Err()
	case Int8Oid:
		return decodeInt8(qr, fd, size), qr.Err()
	case Int2Oid:
		return decodeInt2(qr, fd, size), qr.Err()
	case Int4Oid:
		return decodeInt4(qr, fd, size), qr.Err()
	case VarcharOid, TextOid:
		return decodeText(qr, fd, size), qr.Err()
	case Float4Oid:
		return decodeFloat4(qr, fd, size), qr.Err()
	case Float8Oid:
		return decodeFloat8(qr, fd, size), qr.Err()
	case DateOid:
		return decodeDate(qr, fd, size), qr.Err()
	case TimestampTzOid:
		return decodeTimestampTz(qr, fd, size), qr.Err()
	}

	// if it is not an intrinsic type then return the text
	switch fd.FormatCode {
	case TextFormatCode:
		return qr.MsgReader().ReadString(size), qr.Err()
	// TODO
	//case BinaryFormatCode:
	default:
		return nil, errors.New("Unknown format code")
	}
}

// TODO - document
func (c *Conn) Query(sql string, args ...interface{}) (*QueryResult, error) {
	c.qr = QueryResult{conn: c}
	qr := &c.qr

	// TODO - shouldn't be messing with qr.err and qr.closed directly
	if ps, present := c.preparedStatements[sql]; present {
		qr.fields = ps.FieldDescriptions
		qr.err = c.sendPreparedQuery(ps, args...)
		if qr.err != nil {
			qr.closed = true
		}
		return qr, qr.err
	}

	qr.err = c.sendSimpleQuery(sql, args...)
	if qr.err != nil {
		qr.closed = true
		return qr, qr.err
	}

	// Simple queries don't know the field descriptions of the result.
	// Read until that is known before returning
	for {
		t, r, err := c.rxMsg()
		if err != nil {
			qr.err = err
			qr.closed = true
			return qr, qr.err
		}

		switch t {
		case rowDescription:
			qr.fields = qr.conn.rxRowDescription(r)
			return qr, nil
		default:
			err = qr.conn.processContextFreeMsg(t, r)
			if err != nil {
				qr.closed = true
				qr.err = err
				return qr, qr.err
			}
		}
	}
}

func (c *Conn) QueryRow(sql string, args ...interface{}) *Row {
	qr, _ := c.Query(sql, args...)
	return (*Row)(qr)
}

func (c *Conn) sendQuery(sql string, arguments ...interface{}) (err error) {
	if ps, present := c.preparedStatements[sql]; present {
		return c.sendPreparedQuery(ps, arguments...)
	} else {
		return c.sendSimpleQuery(sql, arguments...)
	}
}

func (c *Conn) sendSimpleQuery(sql string, arguments ...interface{}) (err error) {
	if len(arguments) > 0 {
		sql, err = SanitizeSql(sql, arguments...)
		if err != nil {
			return
		}
	}

	wbuf := newWriteBuf(c.wbuf[0:0], 'Q')
	wbuf.WriteCString(sql)
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)

	return err
}

func (c *Conn) sendPreparedQuery(ps *PreparedStatement, arguments ...interface{}) (err error) {
	if len(ps.ParameterOids) != len(arguments) {
		return fmt.Errorf("Prepared statement \"%v\" requires %d parameters, but %d were provided", ps.Name, len(ps.ParameterOids), len(arguments))
	}

	// bind
	wbuf := newWriteBuf(c.wbuf[0:0], 'B')
	wbuf.WriteByte(0)
	wbuf.WriteCString(ps.Name)

	wbuf.WriteInt16(int16(len(ps.ParameterOids)))
	for i, oid := range ps.ParameterOids {
		switch oid {
		case BoolOid, ByteaOid, Int2Oid, Int4Oid, Int8Oid, Float4Oid, Float8Oid:
			wbuf.WriteInt16(BinaryFormatCode)
		case TextOid, VarcharOid, DateOid, TimestampTzOid:
			wbuf.WriteInt16(TextFormatCode)
		default:
			if _, ok := arguments[i].(BinaryEncoder); ok {
				wbuf.WriteInt16(BinaryFormatCode)
			} else {
				wbuf.WriteInt16(TextFormatCode)
			}
		}
	}

	wbuf.WriteInt16(int16(len(arguments)))
	for i, oid := range ps.ParameterOids {
		if arguments[i] == nil {
			wbuf.WriteInt32(-1)
			continue
		}

		switch oid {
		case BoolOid:
			err = encodeBool(wbuf, arguments[i])
		case ByteaOid:
			err = encodeBytea(wbuf, arguments[i])
		case Int2Oid:
			err = encodeInt2(wbuf, arguments[i])
		case Int4Oid:
			err = encodeInt4(wbuf, arguments[i])
		case Int8Oid:
			err = encodeInt8(wbuf, arguments[i])
		case Float4Oid:
			err = encodeFloat4(wbuf, arguments[i])
		case Float8Oid:
			err = encodeFloat8(wbuf, arguments[i])
		case TextOid, VarcharOid:
			err = encodeText(wbuf, arguments[i])
		case DateOid:
			err = encodeDate(wbuf, arguments[i])
		case TimestampTzOid:
			err = encodeTimestampTz(wbuf, arguments[i])
		default:
			switch arg := arguments[i].(type) {
			case BinaryEncoder:
				err = arg.EncodeBinary(wbuf)
			case TextEncoder:
				var s string
				s, err = arg.EncodeText()
				wbuf.WriteInt32(int32(len(s)))
				wbuf.WriteBytes([]byte(s))
			default:
				return SerializationError(fmt.Sprintf("%T is not a core type and it does not implement TextEncoder or BinaryEncoder", arg))
			}
		}

		if err != nil {
			return err
		}
	}

	wbuf.WriteInt16(int16(len(ps.FieldDescriptions)))
	for _, fd := range ps.FieldDescriptions {
		wbuf.WriteInt16(fd.FormatCode)
	}

	// execute
	wbuf.startMsg('E')
	wbuf.WriteByte(0)
	wbuf.WriteInt32(0)

	// sync
	wbuf.startMsg('S')
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)

	return err
}

// Exec executes sql. sql can be either a prepared statement name or an SQL string.
// arguments will be sanitized before being interpolated into sql strings. arguments
// should be referenced positionally from the sql string as $1, $2, etc.
func (c *Conn) Exec(sql string, arguments ...interface{}) (commandTag CommandTag, err error) {
	startTime := time.Now()

	defer func() {
		if err == nil {
			endTime := time.Now()
			c.logger.Info("Exec", "sql", sql, "args", arguments, "time", endTime.Sub(startTime))
		} else {
			c.logger.Error("Exec", "sql", sql, "args", arguments, "error", err)
		}
	}()

	if err = c.sendQuery(sql, arguments...); err != nil {
		return
	}

	var softErr error

	for {
		var t byte
		var r *MsgReader
		t, r, err = c.rxMsg()
		if err != nil {
			return commandTag, err
		}

		switch t {
		case readyForQuery:
			c.rxReadyForQuery(r)
			return commandTag, softErr
		case rowDescription:
		case dataRow:
		case bindComplete:
		case commandComplete:
			commandTag = CommandTag(r.ReadCString())
		default:
			if e := c.processContextFreeMsg(t, r); e != nil && softErr == nil {
				softErr = e
			}
		}
	}
}

// Transaction runs f in a transaction. f should return true if the transaction
// should be committed or false if it should be rolled back. Return value committed
// is if the transaction was committed or not. committed should be checked separately
// from err as an explicit rollback is not an error. Transaction will use the default
// isolation level for the current connection. To use a specific isolation level see
// TransactionIso
func (c *Conn) Transaction(f func() bool) (committed bool, err error) {
	return c.transaction("", f)
}

// TransactionIso is the same as Transaction except it takes an isoLevel argument that
// it uses as the transaction isolation level.
//
// Valid isolation levels (and their constants) are:
//   serializable (pgx.Serializable)
//   repeatable read (pgx.RepeatableRead)
//   read committed (pgx.ReadCommitted)
//   read uncommitted (pgx.ReadUncommitted)
func (c *Conn) TransactionIso(isoLevel string, f func() bool) (committed bool, err error) {
	return c.transaction(isoLevel, f)
}

func (c *Conn) transaction(isoLevel string, f func() bool) (committed bool, err error) {
	var beginSql string
	if isoLevel == "" {
		beginSql = "begin"
	} else {
		beginSql = fmt.Sprintf("begin isolation level %s", isoLevel)
	}

	if _, err = c.Exec(beginSql); err != nil {
		return
	}
	defer func() {
		if committed && c.TxStatus == 'T' {
			_, err = c.Exec("commit")
			if err != nil {
				committed = false
			}
		} else {
			_, err = c.Exec("rollback")
			committed = false
		}
	}()

	committed = f()
	return
}

// Processes messages that are not exclusive to one context such as
// authentication or query response. The response to these messages
// is the same regardless of when they occur.
func (c *Conn) processContextFreeMsg(t byte, r *MsgReader) (err error) {
	switch t {
	case 'S':
		c.rxParameterStatus(r)
		return nil
	case errorResponse:
		return c.rxErrorResponse(r)
	case noticeResponse:
		return nil
	case notificationResponse:
		c.rxNotificationResponse(r)
		return nil
	default:
		return fmt.Errorf("Received unknown message type: %c", t)
	}
}

func (c *Conn) rxMsg() (t byte, r *MsgReader, err error) {
	if !c.alive {
		return 0, nil, DeadConnError
	}

	t, err = c.mr.rxMsg()
	if err != nil {
		c.die(err)
	}

	return t, &c.mr, err
}

func (c *Conn) rxAuthenticationX(r *MsgReader) (err error) {
	switch r.ReadInt32() {
	case 0: // AuthenticationOk
	case 3: // AuthenticationCleartextPassword
		err = c.txPasswordMessage(c.config.Password)
	case 5: // AuthenticationMD5Password
		salt := r.ReadString(4)
		digestedPassword := "md5" + hexMD5(hexMD5(c.config.Password+c.config.User)+salt)
		err = c.txPasswordMessage(digestedPassword)
	default:
		err = errors.New("Received unknown authentication message")
	}

	return
}

func hexMD5(s string) string {
	hash := md5.New()
	io.WriteString(hash, s)
	return hex.EncodeToString(hash.Sum(nil))
}

func (c *Conn) rxParameterStatus(r *MsgReader) {
	key := r.ReadCString()
	value := r.ReadCString()
	c.RuntimeParams[key] = value
}

func (c *Conn) rxErrorResponse(r *MsgReader) (err PgError) {
	for {
		switch r.ReadByte() {
		case 'S':
			err.Severity = r.ReadCString()
		case 'C':
			err.Code = r.ReadCString()
		case 'M':
			err.Message = r.ReadCString()
		case 0: // End of error message
			if err.Severity == "FATAL" {
				c.die(err)
			}
			return
		default: // Ignore other error fields
			r.ReadCString()
		}
	}
}

func (c *Conn) rxBackendKeyData(r *MsgReader) {
	c.Pid = r.ReadInt32()
	c.SecretKey = r.ReadInt32()
}

func (c *Conn) rxReadyForQuery(r *MsgReader) {
	c.TxStatus = r.ReadByte()
}

func (c *Conn) rxRowDescription(r *MsgReader) (fields []FieldDescription) {
	fieldCount := r.ReadInt16()
	fields = make([]FieldDescription, fieldCount)
	for i := int16(0); i < fieldCount; i++ {
		f := &fields[i]
		f.Name = r.ReadCString()
		f.Table = r.ReadOid()
		f.AttributeNumber = r.ReadInt16()
		f.DataType = r.ReadOid()
		f.DataTypeSize = r.ReadInt16()
		f.Modifier = r.ReadInt32()
		f.FormatCode = r.ReadInt16()
	}
	return
}

func (c *Conn) rxParameterDescription(r *MsgReader) (parameters []Oid) {
	parameterCount := r.ReadInt16()
	parameters = make([]Oid, 0, parameterCount)

	for i := int16(0); i < parameterCount; i++ {
		parameters = append(parameters, r.ReadOid())
	}
	return
}

func (c *Conn) rxNotificationResponse(r *MsgReader) {
	n := new(Notification)
	n.Pid = r.ReadInt32()
	n.Channel = r.ReadCString()
	n.Payload = r.ReadCString()
	c.notifications = append(c.notifications, n)
}

func (c *Conn) startTLS() (err error) {
	err = binary.Write(c.conn, binary.BigEndian, []int32{8, 80877103})
	if err != nil {
		return
	}

	response := make([]byte, 1)
	if _, err = io.ReadFull(c.conn, response); err != nil {
		return
	}

	if response[0] != 'S' {
		err = errors.New("Could not use TLS")
		return
	}

	c.conn = tls.Client(c.conn, c.config.TLSConfig)

	return nil
}

func (c *Conn) txStartupMessage(msg *startupMessage) error {
	_, err := c.conn.Write(msg.Bytes())
	return err
}

func (c *Conn) txPasswordMessage(password string) (err error) {
	wbuf := newWriteBuf(c.wbuf[0:0], 'p')
	wbuf.WriteCString(password)
	wbuf.closeMsg()

	_, err = c.conn.Write(wbuf.buf)

	return err
}

func (c *Conn) die(err error) {
	c.alive = false
	c.causeOfDeath = err
	c.conn.Close()
}
