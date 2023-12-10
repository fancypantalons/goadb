package wire

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"strconv"
	"strings"

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

	return _readWithTimeout(ctx, s.reader, false, readFn)
}

func (s *realScanner) ReadLinesWithTimeout(ctx context.Context) ([]string, error) {
	readFn := func(reader io.Reader, buf []byte) (n int, err error) {
		bufr := bufio.NewReader(reader)

		var line []byte

		if line, _, err = bufr.ReadLine(); err != nil {
			return
		}

		n = len(line)

		copy(buf, line)

		return
	}

	if buf, err := _readWithTimeout(ctx, s.reader, true, readFn); err != nil {
		return nil, err
	} else {
		return strings.Split(string(buf), "\n"), nil
	}
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

	data, err = _readWithTimeout(ctx, r, false, _limitedLengthReaderFunction(length, "message data"))

	return data, err
}

// Reads four bytes from the provided reader and returns them as an ASCII
// encoded string.
func readOctetStringWithTimeout(ctx context.Context, description string, r io.Reader) (string, error) {
	readFn := _limitedLengthReaderFunction(4, description)

	if octet, err := _readWithTimeout(ctx, r, false, readFn); err == nil {
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

	if buf, err = _readWithTimeout(ctx, r, false, readFn); err != nil {
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

	if buf, err = _readWithTimeout(ctx, r, false, readFn); err != nil {
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
func _readWithTimeout(ctx context.Context, r io.Reader, partial bool, readFn _readerFunction) ([]byte, error) {
	var data []byte

	bufferChannel := make(chan *ReadResult)

	// This goroutine is a basic loop that reads buffers until we hit an error,
	// either EOF or some other.
	//
	// How the read is done--as blocks of bytes or as lines--depends on the
	// supplied reader function.
	//
	// This will get aborted if the Context is canceled.
	go func() {
		defer close(bufferChannel)

		for {
			buf := make([]byte, 4096)

			n, e := readFn(r, buf)

			bufferChannel <- &ReadResult{
				buffer: buf,
				length: n,
				err:    e,
			}

			if e != nil {
				return
			}
		}
	}()

	// Here we loop, reading and accumulating buffers until we either
	// finish reading, or the timeout triggers.
	//
	// If the timeout does happen, we either return the amount we've
	// read if partial is true, or we return an error indicating the
	// read was interrupted.
	for {
		select {
		case result := <-bufferChannel:
			data = append(data, result.buffer[:result.length]...)

			if result.err == io.EOF {
				return data, nil
			} else if result.err != nil {
				return nil, result.err
			}

		case <-ctx.Done():
			if partial && len(data) > 0 {
				return data, nil
			} else {
				return nil, errors.WrapErrorf(ctx.Err(), errors.Timeout, "timeout while reading requested data")
			}
		}
	}
}
