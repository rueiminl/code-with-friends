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

func main() {
	fmt.Println("hello world!")
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
	
	msg := []byte("hello")
	_ , err = conn_at_client.Write(msg)
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

