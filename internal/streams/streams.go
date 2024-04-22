package streams

import (
	"fmt"
	"io"
	"net"

	log "github.com/sirupsen/logrus"
)

func CopyIgnoreError(dst io.Writer, src io.Reader) {
	if _, err := io.Copy(dst, src); err != nil {
		log.WithError(err).Debug("We intentionally ignore this error in copying")
	}
}

// block until one side has reached EOF or an error
// return the error of the first side that stopped
func Proxy(a io.ReadWriter, b io.ReadWriter) error {
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(a, b)
		done <- err
	}()
	go func() {
		_, err := io.Copy(b, a)
		done <- err
	}()

	// now we wait until either side is done
	// and return the result of the first side
	return <-done
}

func ForwardSingleConnection(server net.Listener, target string) error {
	local, err := server.Accept()
	if err != nil {
		return fmt.Errorf("local forwarded connection setup failed: %w", err)
	}
	defer local.Close()

	remote, err := net.Dial("tcp", target)
	if err != nil {
		return fmt.Errorf("local forwarded connection to the target failed: %w", err)
	}
	defer remote.Close()

	// end of this function will execute the deferred closes
	return Proxy(local, remote)
}
