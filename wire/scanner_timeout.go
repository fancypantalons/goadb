package wire

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"strconv"

	"github.com/fancypantalons/goadb/utils/errors"
)

type lengthReaderWithTimeout func(context.Context, io.Reader) (int, error)

type _readerFunction func(io.Reader, []byte) (int, error)

type ReadResult struct {
	buffer []byte
	length int
	err    error
}

func (s *realScanner) ReadStatusWithTimeout(ctx context.Context, req string) (string, error) {
	return readStatusFailureAsErrorWithTimeout(ctx, s.reader, req, readHexLengthWithTimeout)
}

func (s *realScanner) ReadUntilEofWithTimeout(ctx context.Context) ([]byte, error) {
	readFn := func(reader io.Reader, buf []byte) (n int, err error) {
		n, err = reader.Read(buf)

		return
	}

	return _readWithTimeout(ctx, s.reader, readFn)
}

func (s *realScanner) ReadOneLineWithTimeout(ctx context.Context) ([]byte, error) {
	readFn := func(reader io.Reader, buf []byte) (n int, err error) {
		bufr := bufio.NewReader(reader)

		var line []byte

		if line, _, err = bufr.ReadLine(); err == nil {
			err = io.EOF
		}

		n = len(line)

		copy(buf, line)

		return
	}

	return _readWithTimeout(ctx, s.reader, readFn)
}

func readStatusFailureAsErrorWithTimeout(ctx context.Context, r io.Reader, req string, messageLengthReader lengthReaderWithTimeout) (string, error) {
	status, err := readOctetStringWithTimeout(ctx, req, r)

	if err != nil {
		return "", errors.WrapErrorf(err, errors.NetworkError, "error reading status for %s", req)
	}

	if isFailureStatus(status) {
		msg, err := readMessageWithTimeout(ctx, r, messageLengthReader)

		if err != nil {
			return "", errors.WrapErrorf(err, errors.NetworkError,
				"server returned error for %s, but couldn't read the error message", req)
		}

		return "", adbServerError(req, string(msg))
	}

	return status, nil
}

// This is a generic function for handling messages that start with a length
// and are followed by a payload.
//
// Then, the read length is used to read that many additional bytes from the
// reader.
func readMessageWithTimeout(ctx context.Context, r io.Reader, lengthReader lengthReaderWithTimeout) ([]byte, error) {
	var err error
	var data []byte

	length, err := lengthReader(ctx, r)

	if err != nil {
		return nil, err
	}

	data, err = _readWithTimeout(ctx, r, _limitedLengthReaderFunction(length, "message data"))

	return data, err
}

// Reads four bytes from the provided reader and returns them as an ASCII
// encoded string.
func readOctetStringWithTimeout(ctx context.Context, description string, r io.Reader) (string, error) {
	readFn := _limitedLengthReaderFunction(4, description)

	if octet, err := _readWithTimeout(ctx, r, readFn); err == nil {
		return string(octet), nil
	} else {
		return "", err
	}
}

// Reads four bytes from the provided reader, treats that as a four byte ASCII
// encoded string representing a hex string, and parses that hex string into an
// integer.
func readHexLengthWithTimeout(ctx context.Context, r io.Reader) (int, error) {
	var buf []byte
	var err error

	readFn := _limitedLengthReaderFunction(4, "length")

	if buf, err = _readWithTimeout(ctx, r, readFn); err != nil {
		return 0, err
	}

	length, err := strconv.ParseInt(string(buf), 16, 64)

	if err != nil {
		return 0, errors.WrapErrorf(err, errors.NetworkError, "could not parse hex length %v", buf)
	}

	return int(length), nil
}

// Reads four bytes from the provided reader and converts them to an int
// assuming little endian encoding.
func readInt32WithTimeout(ctx context.Context, r io.Reader) (int, error) {
	var buf []byte
	var err error

	readFn := _limitedLengthReaderFunction(4, "length")

	if buf, err = _readWithTimeout(ctx, r, readFn); err != nil {
		return 0, err
	} else {
		return int(binary.LittleEndian.Uint32(buf)), nil
	}
}

// A reader function that returns only the number of bytes specified and
// emits an error including the supplied description if something goes
// wrong.
func _limitedLengthReaderFunction(length int, description string) _readerFunction {
	return func(reader io.Reader, buf []byte) (n int, err error) {
		block := make([]byte, length)

		if n, err = io.ReadFull(reader, block); err == nil {
			err = io.EOF
		} else if err == io.ErrUnexpectedEOF {
			err = errIncompleteMessage(description, n, 4)
			return
		} else {
			err = errors.WrapErrorf(err, errors.NetworkError, "error reading "+description)
			return
		}

		copy(buf, block)

		return
	}
}

// Use the supplied function to read from the specified reader within the given
// timeout context.
//
// The reader function returns ReadResult instances with buffers of bytes.
// Reading is complete when the reader function returns an error or io.EOF.
//
// The function returns either the last error received from the reader
// function, or an array of the received bytes.
func _readWithTimeout(ctx context.Context, r io.Reader, readFn _readerFunction) ([]byte, error) {
	readChannel := make(chan *ReadResult)

	go func() {
		var data []byte

		bufferChannel := make(chan *ReadResult)

		// This goroutine is a basic loop that reads buffers until we hit an error,
		// either EOF or some other.
		//
		// How the read is done--as blocks of bytes or as lines--depends on the
		// supplied reader function.
		go func() {
			for {
				buf := make([]byte, 4096)

				n, e := readFn(r, buf)

				bufferChannel <- &ReadResult{
					buffer: buf,
					length: n,
					err:    e,
				}

				if e != nil {
					close(bufferChannel)

					return
				}
			}
		}()

		// Here we loop, reading and accumulating buffers until we either
		// finish reading, or the timeout triggers.
		for {
			select {
			case <-ctx.Done():
				return
			case result := <-bufferChannel:
				data = append(data, result.buffer[:result.length]...)

				if result.err != nil {
					readChannel <- &ReadResult{
						buffer: data,
						length: len(data),
						err:    result.err,
					}

					close(readChannel)

					return
				}
			}
		}
	}()

	select {
	case result := <-readChannel:
		if result.err == nil || result.err == io.EOF {
			return result.buffer[:result.length], nil
		} else {
			return nil, result.err
		}
	case <-ctx.Done():
		return nil, errors.WrapErrorf(ctx.Err(), errors.Timeout, "timeout while reading until EOF")
	}
}
