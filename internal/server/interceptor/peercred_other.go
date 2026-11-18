//go:build !linux && !darwin

package interceptor

import (
	"errors"
	"net"
)

// peerCredentials has nothing to ask where the platform has neither
// SO_PEERCRED nor LOCAL_PEERCRED, so every unix connection resolves as if
// it carried no credentials.
func peerCredentials(*net.UnixConn) (PeerCredentials, error) {
	return PeerCredentials{}, errors.New("interceptor: peer credentials are not available on this platform")
}
