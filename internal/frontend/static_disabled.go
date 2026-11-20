//go:build !homelab_static_ui

package frontend

import "io/fs"

func staticContent() (fs.FS, bool) {
	return nil, false
}
