/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tcpproxy

import (
	"fmt"
	"io"
	"net"

	"k8s.io/klog/v2"

	"pault.ag/go/sniff/parser"
)

// TCPServer describes a server that works in passthrough mode.
type TCPServer struct {
	Hostname      string
	IP            string
	Port          int
	ProxyProtocol bool
}

// TCPProxy describes the passthrough servers and a default as catch all.
type TCPProxy struct {
	ServerList []*TCPServer
	Default    *TCPServer
}

// Get returns the TCPServer to use for a given host.
func (p *TCPProxy) Get(host string) *TCPServer {
	if p.ServerList == nil {
		return p.Default
	}

	for _, s := range p.ServerList {
		if s.Hostname == host {
			return s
		}
	}

	return p.Default
}

// Handle reads enough information from the connection to extract the hostname
// and open a connection to the passthrough server.
func (p *TCPProxy) Handle(conn net.Conn) {
	defer conn.Close()
	// See: https://www.ibm.com/docs/en/ztpf/1.1.0.15?topic=sessions-ssl-record-format
	data := make([]byte, 16384)

	length, err := conn.Read(data)
	if err != nil {
		klog.V(4).ErrorS(err, "Error reading data from the connection")
		return
	}

	proxy := p.Default
	hostname, err := parser.GetHostname(data[:])
	if err == nil {
		klog.V(4).InfoS("TLS Client Hello", "host", hostname)
		proxy = p.Get(hostname)
	}

	if proxy == nil {
		klog.V(4).InfoS("There is no configured proxy for SSL connections.")
		return
	}

	hostPort := net.JoinHostPort(proxy.IP, fmt.Sprintf("%v", proxy.Port))
	klog.V(4).InfoS("passing to", "hostport", hostPort)
	clientConn, err := net.Dial("tcp", hostPort)
	if err != nil {
		klog.V(4).ErrorS(err, "error dialing proxy", "ip", proxy.IP, "port", proxy.Port, "hostname", proxy.Hostname)
		return
	}
	defer clientConn.Close()

	if proxy.ProxyProtocol {
		// write out the Proxy Protocol header
		localAddr := conn.LocalAddr().(*net.TCPAddr)
		remoteAddr := conn.RemoteAddr().(*net.TCPAddr)
		protocol := "UNKNOWN"
		if remoteAddr.IP.To4() != nil {
			protocol = "TCP4"
		} else if remoteAddr.IP.To16() != nil {
			protocol = "TCP6"
		}
		proxyProtocolHeader := fmt.Sprintf("PROXY %s %s %s %d %d\r\n", protocol, remoteAddr.IP.String(), localAddr.IP.String(), remoteAddr.Port, localAddr.Port)
		klog.V(4).InfoS("Writing Proxy Protocol", "header", proxyProtocolHeader)
		_, err = fmt.Fprint(clientConn, proxyProtocolHeader)
	}
	if err != nil {
		klog.ErrorS(err, "Error writing Proxy Protocol header")
		clientConn.Close()
	} else {
		_, err = clientConn.Write(data[:length])
		if err != nil {
			klog.Errorf("Error writing the first 4k of proxy data: %v", err)
			clientConn.Close()
		}
	}

	pipe(clientConn, conn)
}

func pipe(client, server net.Conn) {
	doCopy := func(s, c net.Conn, cancel chan<- bool) {
		if _, err := io.Copy(s, c); err != nil {
			klog.Errorf("Error copying data: %v", err)
		}
		cancel <- true
	}

	cancel := make(chan bool, 2)

	go doCopy(server, client, cancel)
	go doCopy(client, server, cancel)

	<-cancel
}
