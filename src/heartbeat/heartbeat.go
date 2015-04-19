package main

import (
    "fmt"
	"net"
	"os"
)

func CheckError(err error) {
	if err != nil {
		fmt.Println("Error", err)
		os.Exit(0)
	}
}

type Heartbeat struct {
	from map[string]string // key=ip:port; value: name
	to []string // ip:port
	socket *net.UDPConn
}

func (this *Heartbeat) Initialize(host string) {
	addr, err := net.ResolveUDPAddr("udp", host)
	CheckError(err)
	this.socket, err = net.ListenUDP("udp", addr)
	CheckError(err)
}

func (this *Heartbeat) SendTo(host string) {
	addr, err := net.ResolveUDPAddr("udp", host)
	CheckError(err)
	this.socket.WriteToUDP([]byte("hello"), addr)
}

func (this *Heartbeat) RecvFrom() {
	buf := make([]byte, 1024)
	n, addr, err := this.socket.ReadFromUDP(buf)
    if err != nil {
        fmt.Println("Error: ",err)
    } 
	fmt.Println("Received", n, "bytes:", string(buf[0:n]), "from", addr)
}



func main() {
	heartbeat_sample()
	udp_sample()
}

func heartbeat_sample() {
	h1 := new(Heartbeat)
	h1.Initialize("127.0.0.1:10002")
	h2 := new(Heartbeat)
	h2.Initialize("127.0.0.1:10003")
	h1.SendTo("127.0.0.1:10003")
	h2.RecvFrom()
}

func udp_sample() {
	server_addr, err := net.ResolveUDPAddr("udp", ":10001")
	CheckError(err)

	conn_at_server, err := net.ListenUDP("udp", server_addr)
	CheckError(err)
	defer conn_at_server.Close()
	
	client_addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	CheckError(err)
	
	conn_at_client, err := net.DialUDP("udp", client_addr, server_addr)
	CheckError(err)
	defer conn_at_client.Close()
	
	// msg := []byte("hello")
	_ , err = conn_at_client.Write([]byte("hello"))
	if err != nil {
        fmt.Println("Error", err)
	}
	
	buf := make([]byte, 1024)
	n, addr, err := conn_at_server.ReadFromUDP(buf)
    if err != nil {
        fmt.Println("Error: ",err)
    } 
	fmt.Println("Received", n, "bytes:", string(buf[0:n]), "from", addr)
}