package agents

import "net"

func dialUnix(sock string) (net.Conn, error) { return net.Dial("unix", sock) }
