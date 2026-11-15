//go:build linux

package sandbox

import (
	"net"
	"testing"
)

// listen starts a local TCP server that accepts and closes connections and
// returns its address.
func listen(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return lis.Addr().String()
}
