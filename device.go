package adb

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/fancypantalons/goadb/utils/errors"
	"github.com/fancypantalons/goadb/wire"
)

// MtimeOfClose should be passed to OpenWrite to set the file modification time to the time the Close
// method is called.
var MtimeOfClose = time.Time{}

// Device communicates with a specific Android device.
// To get an instance, call Device() on an Adb.
type Device struct {
	server     server
	descriptor DeviceDescriptor

	// Used to get device info.
	deviceListFunc func() ([]*DeviceInfo, error)
}

func (c *Device) String() string {
	return c.descriptor.String()
}

// get-product is documented, but not implemented, in the server.
// TODO(z): Make product exported if get-product is ever implemented in adb.
func (c *Device) product() (string, error) {
	attr, err := c.getAttribute("get-product")
	return attr, wrapClientError(err, c, "Product")
}

func (c *Device) Serial() (string, error) {
	attr, err := c.getAttribute("get-serialno")
	return attr, wrapClientError(err, c, "Serial")
}

func (c *Device) DevicePath() (string, error) {
	attr, err := c.getAttribute("get-devpath")
	return attr, wrapClientError(err, c, "DevicePath")
}

func (c *Device) State() (DeviceState, error) {
	attr, err := c.getAttribute("get-state")
	if err != nil {
		if strings.Contains(err.Error(), "unauthorized") {
			return StateUnauthorized, nil
		}
		return StateInvalid, wrapClientError(err, c, "State")
	}
	state, err := parseDeviceState(attr)
	return state, wrapClientError(err, c, "State")
}

func (c *Device) DeviceInfo() (*DeviceInfo, error) {
	// Adb doesn't actually provide a way to get this for an individual device,
	// so we have to just list devices and find ourselves.

	serial, err := c.Serial()
	if err != nil {
		return nil, wrapClientError(err, c, "GetDeviceInfo(GetSerial)")
	}

	devices, err := c.deviceListFunc()
	if err != nil {
		return nil, wrapClientError(err, c, "DeviceInfo(ListDevices)")
	}

	for _, deviceInfo := range devices {
		if deviceInfo.Serial == serial {
			return deviceInfo, nil
		}
	}

	err = errors.Errorf(errors.DeviceNotFound, "device list doesn't contain serial %s", serial)
	return nil, wrapClientError(err, c, "DeviceInfo")
}

/*
RunCommand runs the specified commands on a shell on the device.

From the Android docs:

	Run 'command arg1 arg2 ...' in a shell on the device, and return
	its output and error streams. Note that arguments must be separated
	by spaces. If an argument contains a space, it must be quoted with
	double-quotes. Arguments cannot contain double quotes or things
	will go very wrong.

	Note that this is the non-interactive version of "adb shell"

Source: https://android.googlesource.com/platform/system/core/+/master/adb/SERVICES.TXT

This method quotes the arguments for you, and will return an error if any of them
contain double quotes.
*/
func (c *Device) RunCommand(cmd string, args ...string) (string, error) {
	cmd, err := prepareCommandLine(cmd, args...)
	if err != nil {
		return "", wrapClientError(err, c, "RunCommand")
	}

	conn, err := c.dialDevice()
	if err != nil {
		return "", wrapClientError(err, c, "RunCommand")
	}
	defer conn.Close()

	req := fmt.Sprintf("shell:%s", cmd)

	// Shell responses are special, they don't include a length header.
	// We read until the stream is closed.
	// So, we can't use conn.RoundTripSingleResponse.
	if err = conn.SendMessage([]byte(req)); err != nil {
		return "", wrapClientError(err, c, "RunCommand")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err = conn.ReadStatusWithTimeout(ctx, req); err != nil {
		return "", wrapClientError(err, c, "RunCommand")
	}

	resp, err := conn.ReadUntilEofWithTimeout(ctx)
	if err != nil {
		fmt.Println("RunCommand, ReadUntilEof done, err:", ErrorWithCauseChain(err))
	}

	return string(resp), wrapClientError(err, c, "RunCommand")
}

// Root restart adbd with root permissions
func (c *Device) Root() (string, error) {
	conn, err := c.dialDevice()
	if err != nil {
		return "", wrapClientError(err, c, "Root")
	}
	defer conn.Close()

	//resp, err := conn.RoundTripSingleResponse([]byte("root"))
	if err = conn.SendMessage([]byte("root:")); err != nil {
		return "", wrapClientError(err, c, "Root")
	}
	if _, err = conn.ReadStatus("root:"); err != nil {
		return "", wrapClientError(err, c, "Root")
	}

	resp, err := conn.ReadUntilEof()

	// The server needs to restart. If we don't sleep we seem to
	// connect but in an unstable way (i.e. the next commands would fail unpredictably)
	//TODO there must be some state we can look for rather than just waiting.
	time.Sleep(5 * time.Second)

	return string(resp), wrapClientError(err, c, "Root")
}

// Forward Use the forward command to set up arbitrary port forwarding,
// which forwards requests on a specific host port to a different port on a device.
func (c *Device) Forward(hostPort string, devicePort string) (string, error) {
	conn, err := c.dialDevice()
	if err != nil {
		return "", wrapClientError(err, c, "Forward")
	}
	defer conn.Close()

	serial, err := c.Serial()
	if err != nil {
		return "", wrapClientError(err, c, "Forward")
	}

	if err = conn.SendMessage([]byte("host-serial:" + serial + ":forward:" + hostPort + ";" + devicePort)); err != nil {
		return "", wrapClientError(err, c, "Forward")
	}
	if _, err = conn.ReadStatus("forward"); err != nil {
		return "", wrapClientError(err, c, "Forward")
	}

	resp, err := conn.ReadUntilEof()
	return string(resp), wrapClientError(err, c, "Forward")
}

// ListForwards Used to list all the curretly forwarded ports
func (c *Device) ListForwards(hostPort string) (string, error) {

	conn, err := c.dialDevice()
	if err != nil {
		return "", wrapClientError(err, c, "ListForwards")
	}
	defer conn.Close()

	serial, err := c.Serial()
	if err != nil {
		return "", wrapClientError(err, c, "ListForwards")
	}

	//first see if this device is still forwarded
	list, err := conn.RoundTripSingleResponse([]byte("host-serial:" + serial + ":list-forward"))

	if err != nil {
		return "", wrapClientError(err, c, "ListForwards")
	}

	return string(list), nil
}

// RemoveForward Used to remove a specific forward socket connection.
func (c *Device) RemoveForward(hostPort string) (string, error) {

	list, err := c.ListForwards(hostPort)

	if err != nil {
		return "", wrapClientError(err, c, "RemoveForward")
	}

	conn, err := c.dialDevice()
	if err != nil {
		return "", wrapClientError(err, c, "RemoveForward")
	}
	defer conn.Close()

	serial, err := c.Serial()
	if err != nil {
		return "", wrapClientError(err, c, "RemoveForward")
	}

	if strings.Contains(string(list), serial) {

		if err = conn.SendMessage([]byte("host-serial:" + serial + ":killforward:" + hostPort)); err != nil {
			return "", wrapClientError(err, c, "RemoveForward")
		}

		if _, err = conn.ReadStatus("forward"); err != nil {
			return "", wrapClientError(err, c, "RemoveForward")
		}

		resp, err := conn.ReadUntilEof()

		return string(resp), wrapClientError(err, c, "RemoveForward")
	} else {
		return "", nil
	}
}

/*
Remount, from the official adb command’s docs:

	Ask adbd to remount the device's filesystem in read-write mode,
	instead of read-only. This is usually necessary before performing
	an "adb sync" or "adb push" request.
	This request may not succeed on certain builds which do not allow
	that.

Source: https://android.googlesource.com/platform/system/core/+/master/adb/SERVICES.TXT
*/
func (c *Device) Remount() (string, error) {
	conn, err := c.dialDevice()
	if err != nil {
		return "", wrapClientError(err, c, "Remount")
	}
	defer conn.Close()

	resp, err := conn.RoundTripSingleResponse([]byte("remount"))
	return string(resp), wrapClientError(err, c, "Remount")
}

func (c *Device) ListDirEntries(path string) (*DirEntries, error) {
	conn, err := c.getSyncConn()
	if err != nil {
		return nil, wrapClientError(err, c, "ListDirEntries(%s)", path)
	}

	entries, err := listDirEntries(conn, path)
	return entries, wrapClientError(err, c, "ListDirEntries(%s)", path)
}

func (c *Device) Stat(path string) (*DirEntry, error) {
	conn, err := c.getSyncConn()
	if err != nil {
		return nil, wrapClientError(err, c, "Stat(%s)", path)
	}
	defer conn.Close()

	entry, err := stat(conn, path)
	return entry, wrapClientError(err, c, "Stat(%s)", path)
}

func (c *Device) OpenRead(path string) (io.ReadCloser, error) {
	conn, err := c.getSyncConn()
	if err != nil {
		return nil, wrapClientError(err, c, "OpenRead(%s)", path)
	}

	reader, err := receiveFile(conn, path)
	return reader, wrapClientError(err, c, "OpenRead(%s)", path)
}

// OpenWrite opens the file at path on the device, creating it with the permissions specified
// by perms if necessary, and returns a writer that writes to the file.
// The files modification time will be set to mtime when the WriterCloser is closed. The zero value
// is TimeOfClose, which will use the time the Close method is called as the modification time.
func (c *Device) OpenWrite(path string, perms os.FileMode, mtime time.Time) (io.WriteCloser, error) {
	conn, err := c.getSyncConn()
	if err != nil {
		return nil, wrapClientError(err, c, "OpenWrite(%s)", path)
	}

	writer, err := sendFile(conn, path, perms, mtime)
	return writer, wrapClientError(err, c, "OpenWrite(%s)", path)
}

// Using the 'jdwp' command, return a list of PIDs that have an available
// JDWP endpoint.
//
// Note, the 'jdwp' command is odd in that rather than just finding PIDs
// and then immediately returning, it just sits there returning new IDs
// as they become available.  That's nice if you run this command before
// a debuggable app has started, but it means it doesn't actually, you know,
// stop.
//
// So this call includes a timeout where it'll wait for the indicated time
// and then terminate the command and return the set of found PIDs.
func (c *Device) GetDebuggablePids(timeout time.Duration) ([]string, error) {
	conn, err := c.dialDevice()
	if err != nil {
		return nil, wrapClientError(err, c, "jdwp")
	}
	defer conn.Close()

	req := "jdwp"

	// The jdwp command doesn't terminate normally.  Instead it sits there
	// returning PIDs.  So we have to run it, read what we can, then time
	// out.
	if err = conn.SendMessage([]byte(req)); err != nil {
		return nil, wrapClientError(err, c, "jdwp")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var status string

	if status, err = conn.ReadStatusWithTimeout(ctx, req); err != nil {
		return nil, wrapClientError(err, c, "jdwp")
	}

	if status != wire.StatusSuccess {
		return nil, errors.AssertionErrorf("jdwp returns bad status: %s", status)
	}

	fmt.Println("Aight, we good, reading")

	return conn.ReadLinesWithTimeout(ctx)
}

// getAttribute returns the first message returned by the server by running
// <host-prefix>:<attr>, where host-prefix is determined from the DeviceDescriptor.
func (c *Device) getAttribute(attr string) (string, error) {
	resp, err := roundTripSingleResponse(c.server,
		fmt.Sprintf("%s:%s", c.descriptor.getHostPrefix(), attr))
	if err != nil {
		return "", err
	}
	return string(resp), nil
}

func (c *Device) getSyncConn() (*wire.SyncConn, error) {
	conn, err := c.dialDevice()
	if err != nil {
		return nil, err
	}

	// Switch the connection to sync mode.
	if err := wire.SendMessageString(conn, "sync:"); err != nil {
		return nil, err
	}
	if _, err := conn.ReadStatus("sync"); err != nil {
		return nil, err
	}

	return conn.NewSyncConn(), nil
}

// dialDevice switches the connection to communicate directly with the device
// by requesting the transport defined by the DeviceDescriptor.
func (c *Device) dialDevice() (*wire.Conn, error) {
	conn, err := c.server.Dial()
	if err != nil {
		return nil, err
	}

	req := fmt.Sprintf("host:%s", c.descriptor.getTransportDescriptor())
	if err = wire.SendMessageString(conn, req); err != nil {
		conn.Close()
		return nil, errors.WrapErrf(err, "error connecting to device '%s'", c.descriptor)
	}

	if _, err = conn.ReadStatus(req); err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}

// prepareCommandLine validates the command and argument strings, quotes
// arguments if required, and joins them into a valid adb command string.
func prepareCommandLine(cmd string, args ...string) (string, error) {
	if isBlank(cmd) {
		return "", errors.AssertionErrorf("command cannot be empty")
	}

	for i, arg := range args {
		if strings.ContainsRune(arg, '"') {
			return "", errors.Errorf(errors.ParseError, "arg at index %d contains an invalid double quote: %s", i, arg)
		}
		if containsWhitespace(arg) {
			args[i] = fmt.Sprintf("\"%s\"", arg)
		}
	}

	// Prepend the command to the args array.
	if len(args) > 0 {
		cmd = fmt.Sprintf("%s %s", cmd, strings.Join(args, " "))
	}

	return cmd, nil
}
