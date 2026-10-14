//go:build windows

package platform

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The agent runs in the user's Windows session (the logon task). Its capture
// and input need that session's desktop to be the one on a screen and to
// take input: not while the session is locked or shows a secure desktop (the
// lock screen, a UAC prompt, Ctrl+Alt+Del: the Winlogon desktop, which only
// SYSTEM may open), and not while it is disconnected (a remote desktop client
// closed it: nothing draws its desktop, and the console shows the sign-in
// screen of another session). "tscon <session> /dest:console" moves a
// disconnected session to the console, unlocked; WTSConnectSessionW is the
// same call.

var (
	wtsapi32                        = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformationW = wtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory               = wtsapi32.NewProc("WTSFreeMemory")
	procWTSConnectSessionW          = wtsapi32.NewProc("WTSConnectSessionW")
	procOpenInputDesktop            = user32.NewProc("OpenInputDesktop")
	procCloseDesktop                = user32.NewProc("CloseDesktop")
	procGetUserObjectInformationW   = user32.NewProc("GetUserObjectInformationW")
)

const (
	wtsUserName         = 5   // WTS_INFO_CLASS WTSUserName
	wtsConnectState     = 8   // WTS_INFO_CLASS WTSConnectState
	wtsSessionInfoEx    = 25  // WTS_INFO_CLASS WTSSessionInfoEx
	wtsDisconnected     = 4   // WTS_CONNECTSTATE_CLASS WTSDisconnected
	wtsSessionStateLock = 0   // WTSINFOEX_LEVEL1 SessionFlags WTS_SESSIONSTATE_LOCK (Windows 8 and later)
	desktopReadObjects  = 0x1 // DESKTOP_READOBJECTS
	uoiName             = 2   // UOI_NAME
	noSession           = ^uint32(0)
)

// wtsQuery returns a session's WTSQuerySessionInformationW buffer of class
// class, copied.
func wtsQuery(session uint32, class uintptr) ([]byte, error) {
	if err := procWTSQuerySessionInformationW.Find(); err != nil {
		return nil, err
	}
	var buf *byte
	var n uint32
	r, _, err := procWTSQuerySessionInformationW.Call(0 /*WTS_CURRENT_SERVER_HANDLE*/, uintptr(session), class,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || buf == nil {
		return nil, err
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buf)))
	return append([]byte(nil), unsafe.Slice(buf, n)...), nil
}

// sessionUser is the user name signed in to session ("" for the sign-in
// screen's session, or when it cannot be read).
func sessionUser(session uint32) string {
	b, err := wtsQuery(session, wtsUserName)
	if err != nil || len(b) < 2 {
		return ""
	}
	return windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), len(b)/2))
}

// DesktopState reads this process's Windows session and input desktop.
func DesktopState() Desktop {
	d := Desktop{Console: windows.WTSGetActiveConsoleSessionId(), Remote: RemoteSession()}
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &d.Session); err != nil {
		return d
	}
	// WTSINFOEXW: Level (4 bytes), then the 8-byte aligned union whose
	// WTSINFOEX_LEVEL1_W starts with SessionId, SessionState, SessionFlags.
	if b, err := wtsQuery(d.Session, wtsSessionInfoEx); err == nil && len(b) >= 20 && *(*uint32)(unsafe.Pointer(&b[0])) == 1 {
		d.Known = true
		d.Disconnected = *(*uint32)(unsafe.Pointer(&b[12])) == wtsDisconnected
		d.Locked = *(*int32)(unsafe.Pointer(&b[16])) == wtsSessionStateLock
	} else if b, err := wtsQuery(d.Session, wtsConnectState); err == nil && len(b) >= 4 {
		d.Known = true
		d.Disconnected = *(*uint32)(unsafe.Pointer(&b[0])) == wtsDisconnected
	}
	if procOpenInputDesktop.Find() != nil {
		return d
	}
	h, _, err := procOpenInputDesktop.Call(0, 0, desktopReadObjects)
	if h == 0 {
		// The Winlogon desktop (the lock screen, a UAC prompt) is SYSTEM's
		// only: an elevated agent cannot open it either.
		d.Secure = errors.Is(err, windows.ERROR_ACCESS_DENIED)
		return d
	}
	defer procCloseDesktop.Call(h)
	var name [64]uint16
	var n uint32
	if r, _, _ := procGetUserObjectInformationW.Call(h, uoiName, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)*2),
		uintptr(unsafe.Pointer(&n))); r != 0 {
		d.Input = windows.UTF16ToString(name[:])
		d.Secure = !strings.EqualFold(d.Input, "Default")
	}
	return d
}

// ConnectToConsole moves this process's Windows session to the console, as
// "tscon <session> /dest:console" does: a session left disconnected by a
// remote desktop client gets the console's display (and comes back
// unlocked). It refuses when another user is signed in at the console (that
// session would be disconnected) and does nothing when the session is the
// console's already. It needs the right to connect sessions: an elevated
// administrator (the logon task's RunLevel Highest) has it.
func ConnectToConsole() error {
	var self uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &self); err != nil {
		return err
	}
	console := windows.WTSGetActiveConsoleSessionId()
	switch {
	case console == noSession:
		return errors.New("no session is attached to the console right now")
	case console == self:
		return nil
	}
	if u := sessionUser(console); u != "" && !strings.EqualFold(u, sessionUser(self)) {
		return fmt.Errorf("%s is signed in at the console", u)
	}
	if err := procWTSConnectSessionW.Find(); err != nil {
		return err
	}
	var empty uint16 // the password: empty for the same user
	if r, _, err := procWTSConnectSessionW.Call(uintptr(self), uintptr(console), uintptr(unsafe.Pointer(&empty)), 1); r == 0 {
		return fmt.Errorf("WTSConnectSession %d to the console (session %d): %w", self, console, err)
	}
	return nil
}
