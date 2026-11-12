package main

import (
	"context"
	"errors"
	"io"

	"github.com/sylvinus/agent-vm/internal/hostproxy"
)

// Private subprocess entry for host installers. Proxy settings are resolved in
// Go for every HTTPS request, including redirects and PAC host rules.
func hostDownload(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 3 && len(args) != 4 && len(args) != 6 {
		return errors.New("host-download requires URL, staging path, component name and optional Accept, cache directory and SHA-256")
	}
	var accept, cache, digest string
	if len(args) >= 4 {
		accept = args[3]
	}
	if len(args) == 6 {
		cache, digest = args[4], args[5]
	}
	ctx, cancel, err := hostDownloadContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	return hostproxy.Download(ctx, args[0], args[1], args[2], accept, cache, digest, out)
}
