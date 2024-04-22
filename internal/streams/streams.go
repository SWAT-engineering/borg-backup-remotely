package streams

import (
	"io"
	"net"

	log "github.com/sirupsen/logrus"
)

func CopyIgnoreError(dst io.Writer, src io.Reader) {
	if _, err := io.Copy(dst, src); err != nil {
		log.WithError(err).Debug("We intentionally ignore this error in copying")
	}
}

func Proxy(a io.ReadWriter, b io.ReadWriter) {
	done := make(chan bool, 2)
	go func() {
		CopyIgnoreError(a, b)
		done <- true
	}()
	go func() {
		CopyIgnoreError(b, a)
		done <- true
	}()

	// now we wait until either side is done
	<-done
}

func ForwardSingleConnection(server net.Listener, target string) {
	local, err := server.Accept()
	if err != nil {
		log.WithError(err).Error("local forwarded connection failed")
		return
	}
	defer local.Close()

	remote, err := net.Dial("tcp", target)
	if err != nil {
		log.WithError(err).Error("local forwarded connection failed (forward connection)")
		return
	}
	defer remote.Close()

	Proxy(local, remote)
	// end of this function will execute the deferred closes
}
