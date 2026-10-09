package encoder

import (
	"fmt"
	"os"
	"path/filepath"
)

// The libavcodec backend (GUIDE 3.8; docs/HELPER_PROTOCOL.md "libavcodec
// encoder backend") loads FFmpeg 8.x's shared DLLs at run time from
// Options.FFmpegDir.

// LavcDirName is the directory next to recon-encoder.exe (and recon-host.exe)
// where install-host.ps1 -InstallLibavcodec puts those DLLs.
const LavcDirName = "ffmpeg-lgpl"

// LavcLibraries are the DLLs the helper looks for in that directory:
// libavcodec 62 and libavutil 60 (FFmpeg 8.x, whose struct layouts it was
// built against); avcodec-62.dll needs swresample-6.dll next to it.
var LavcLibraries = []string{"avcodec-62.dll", "avutil-60.dll"}

// LavcMissing returns why the libavcodec backend cannot find its libraries in
// dir ("" when they are there): the first one missing, and how to install
// them. The helper checks more when it loads them (versions, exports); its
// caps' Unavailable["lavc"] says why the backend is unusable after all.
func LavcMissing(dir string) string {
	for _, name := range LavcLibraries {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.IsDir() {
			return fmt.Sprintf("%s has no %s (install-host.ps1 -InstallLibavcodec installs FFmpeg's LGPL libraries there)", dir, name)
		}
	}
	return ""
}
