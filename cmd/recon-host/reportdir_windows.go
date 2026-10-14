package main

import "golang.org/x/sys/windows"

// desktopDirs are the user's desktop folders to try, in order: the Desktop
// known folder (where Explorer shows it, OneDrive's or a redirected one),
// then %USERPROFILE%\Desktop.
func desktopDirs() []string {
	var dirs []string
	if d, err := windows.KnownFolderPath(windows.FOLDERID_Desktop, 0); err == nil && d != "" {
		dirs = append(dirs, d)
	}
	return append(dirs, homeDesktop()...)
}
