package spdy

import (
	"bufio"
	"bytes"
	"fmt"
	"http"
	"os"
	"sync"
)

const (
	maxPriorities     = 8
	defaultBufferSize = 64 * 1024
	defaultWindow     = 64 * 1024
	maxStreamId       = 0x7FFFFFFF
)

var (
	ErrProtocolError          = os.NewError("spdy: protocol error")
	ErrInvalidStream          = os.NewError("spdy: invalid stream")
	ErrRefusedStream          = os.NewError("spdy: stream refused")
	ErrUnsupportedVersion     = os.NewError("spdy: unsupported version")
	ErrCancel                 = os.NewError("spdy: cancel")
	ErrFlowControl            = os.NewError("spdy: flow control error")
	ErrStreamInUse            = os.NewError("spdy: stream in use")
	ErrStreamAlreadyClosed    = os.NewError("spdy: stream closed")
	ErrTooManyStreams         = os.NewError("spdy: too many streams")
	ErrAssociatedStreamClosed = os.NewError("spdy: associated stream closed")
	ErrWriteAfterClose        = os.NewError("spdy: attempt to write to closed stream")
)

type stream struct {
	streamId   int
	connection *connection

	lock sync.Mutex
	cond *sync.Cond // trigger when locked data is changed

	// access is locked
	response *http.Response
	request  *http.Request

	// access is locked, write from dispatch, read on stream rx
	rxFinished bool // frame with FLAG_FIN has been received
	rxBuffer   *bytes.Buffer

	// access from stream tx thread
	txClosed        bool // streamTx.Close has been called
	txFinished      bool // frame with FLAG_FIN has been sent
	txPriority      int
	shouldSendReply bool // if SYN_REPLY still needs to be sent
	headerWritten   bool // streamTx.WriteHeader has been called
	replyHeader     http.Header
	replyStatus     int

	txWindow int // access is locked, session rx and stream tx threads
	txBuffer *bufio.Writer

	closeError   os.Error // write access is locked
	closeChannel chan bool

	children []*stream // access is locked by streamLock
	parent   *stream

	handler http.Handler // handler for pushed associated streams where we are the requestor
}

// Split the user accessible stream methods into seperate sets

// Used as the txBuffer output
type streamTxOut stream

// Given to the user when they can write as the ResponseWriter.
// The user can then cast to a http.RoundTrip to push associated requests.
type streamTx stream

var _ http.ResponseWriter = (*streamTx)(nil)
var _ http.RoundTripper = (*streamTx)(nil)

// Given to the user when the can read
type streamRx stream

// Read reads request/response data.
//
// This is called by the resp.Body.Read by the user after starting a request.
//
// It is also called by the user to get request data in request.Body.Read.
//
// This will return os.EOF when all data has been successfully read without
// getting a SPDY RST_STREAM (equivalent of an abort).
func (s *streamRx) Read(buf []byte) (int, os.Error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	for !s.rxFinished && (s.rxBuffer == nil || s.rxBuffer.Len() == 0) && s.closeError == nil {
		s.cond.Wait()
	}

	if s.closeError != nil {
		return 0, s.closeError
	}

	if s.rxBuffer == nil {
		return 0, os.EOF
	}

	// This returns os.EOF if we read with no data due to s.rxClosed ==
	// true
	return s.rxBuffer.Read(buf)
}

// Closes the rx channel
//
// TODO(james): Currently we do nothing. Should we send a refused or cancel?
func (s *streamRx) Close() os.Error {
	return nil
}

// RoundTrip starts a new pushed request associated with this request.
//
// To change the priority of the request set the ":priority" header field to a
// number between 0 (highest) and MaxPriority-1 (lowest). Otherwise
// DefaultPriority will be used.
//
// To start an unidirectional request where we do not wait for the response,
// set the ":unidirectional" header to a non empty value. The return value
// resp will then be nil.
func (s *streamTx) RoundTrip(req *http.Request) (resp *http.Response, err os.Error) {
	return s.connection.startRequest(req, (*stream)(s), nil)
}

// Header returns the response header so that headers can be changed.
//
// The header should not be altered after WriteHeader or Write has been
// called.
func (s *streamTx) Header() http.Header {
	if s.replyHeader == nil {
		s.replyHeader = make(http.Header)
	}
	return s.replyHeader
}

// WriteHeader writes the response header.
//
// The header will be buffered until the next Flush, the handler function
// returns or when the tx buffer fills up.
//
// The Header() should not be changed after calling this.
func (s *streamTx) WriteHeader(status int) {
	s.headerWritten = true
	s.replyStatus = status
}

// Write writes response body data.
//
// This will call WriteHeader if it hasn't been already called.
//
// The data will be buffered and then actually sent the next time Flush is
// called, when the handler function returns, or when the tx buffer fills up.
//
// This function is also used by the request tx pump to send request body
// data.
func (s *streamTx) Write(data []byte) (int, os.Error) {
	if s.txClosed {
		return 0, ErrWriteAfterClose
	}

	if !s.headerWritten {
		s.WriteHeader(http.StatusOK)
	}

	if s.txBuffer == nil {
		s.txBuffer = bufio.NewWriter((*streamTxOut)(s))
	}

	return s.txBuffer.Write(data)
}

// Close closes the tx pipe and flushes any buffered data.
func (s *streamTx) Close() os.Error {
	if s.txClosed {
		return ErrWriteAfterClose
	}

	s.txClosed = true

	if err := s.Flush(); err != nil {
		return err
	}

	// In most cases the close will have already been sent with the last
	// of the data as it got flushed through, but in cases where no data
	// was buffered (eg if it already been flushed or we never sent any)
	// then we send an empty data frame with the finished flag here.
	if !s.txFinished {
		return nil
	}

	f := dataFrame{
		Finished: true,
		StreamId: s.streamId,
	}

	if err := (*streamTxOut)(s).sendFrame(f); err != nil {
		return err
	}

	s.txFinished = true
	s.connection.onFinishedSent <- (*stream)(s)
	return nil

}

// Flush flushes data being written to the sessions tx thread which flushes it
// out the socket.
func (s *streamTx) Flush() os.Error {
	if s.shouldSendReply {
		if err := (*streamTxOut)(s).sendReply(); err != nil {
			return err
		}
	}

	if s.txBuffer != nil {
		if err := s.txBuffer.Flush(); err != nil {
			return err
		}
	}

	return nil
}

// sendFrame sends a frame to the session tx thread, which sends it out the
// socket.
func (s *streamTxOut) sendFrame(f frame) os.Error {
	select {
	case <-s.closeChannel:
		return s.closeError
	case s.connection.sendData[s.txPriority] <- f:
	}

	return nil
}

// sendReply sends the SYN_REPLY frame which contains the response headers.
// Note this won't be called until the first flush or the tx channel is closed.
func (s *streamTxOut) sendReply() os.Error {
	s.shouldSendReply = false

	f := &synReplyFrame{
		Finished: s.txClosed,
		StreamId: s.streamId,
		Header:   s.replyHeader,
		Status:   fmt.Sprintf("%d", s.replyStatus),
		Proto:    "http/1.1",
	}

	if err := s.sendFrame(f); err != nil {
		return err
	}

	if f.Finished {
		s.txFinished = true
		s.connection.onFinishedSent <- (*stream)(s)
	}

	return nil
}

// amountOfDataToSend figures out how much data we can send, potentially
// waiting for a WINDOW_UPDATE frame from the remote. It only returns once we
// can send > 0 bytes or the remote sent a RST_STREAM to abort.
func (s *streamTxOut) amountOfDataToSend(want int) (int, os.Error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	for s.txWindow <= 0 && s.closeError != nil {
		s.cond.Wait()
	}

	if s.closeError != nil {
		return 0, s.closeError
	}

	tosend := want
	if tosend < s.txWindow {
		tosend = s.txWindow
	}

	s.txWindow -= tosend
	return tosend, nil
}

// Function hooked up to the output of s.txBuffer to flush data to the session
// tx thread.
func (s *streamTxOut) Write(data []byte) (int, os.Error) {
	// If this is the first call and is due to the tx buffer filling up,
	// then the reply hasn't yet been sent.
	if s.shouldSendReply {
		if err := s.sendReply(); err != nil {
			return 0, err
		}
	}

	sent := 0
	for sent < len(data) {
		tosend, err := s.amountOfDataToSend(len(data) - sent)
		if err != nil {
			return sent, err
		}

		f := dataFrame{
			Finished: s.txClosed && sent+tosend == len(data),
			Data:     data[sent : sent+tosend],
			StreamId: s.streamId,
		}

		if err := s.sendFrame(f); err != nil {
			return sent, err
		}

		if f.Finished {
			s.txFinished = true
			s.connection.onFinishedSent <- (*stream)(s)
		}

		sent += tosend
	}

	return sent, nil
}