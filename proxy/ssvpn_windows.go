package proxy

import (
	"github.com/alphabetY/goflyway2/pkg/trafficmon"
	"net"
)

func vpnDial(address string) (net.Conn, error) {
	panic("not on Windows")
}

func sendTrafficStats(stat *trafficmon.Survey) error {
	return nil
}
