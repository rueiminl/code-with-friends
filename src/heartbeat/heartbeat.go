package main

import (
    "fmt"
	"net"
	"os"
	"time"
	"sync"
)

const (
	DEAD_TIMEOUT = 90
	HEARTBEAT_TIMEOUT = 30
	DEAD_BUFFER = 3
)

func CheckError(err error) {
	if err != nil {
		fmt.Println("Error", err)
		os.Exit(0)
	}
}

type Heartbeat struct {
	from []string // ip:port
	to []*net.UDPAddr   // ip:port
	host string
	socket *net.UDPConn
	ts map[string]time.Time
	mutex *sync.Mutex
	dead chan string
}

func (this *Heartbeat) GetDeadChan() chan string {
	return this.dead
}

func (this *Heartbeat) Initialize(host string, from []string, to []string) {
	this.host = host
	addr, err := net.ResolveUDPAddr("udp", host)
	CheckError(err)
	this.socket, err = net.ListenUDP("udp", addr)
	CheckError(err)
	this.from = from
	this.to = make([]*net.UDPAddr, len(to))
	for i, host := range to {
		addr, err := net.ResolveUDPAddr("udp", host)
		CheckError(err)
		this.to[i] = addr
	}
	this.mutex = new(sync.Mutex)
	this.ts = make(map[string]time.Time)
	this.dead = make(chan string, DEAD_BUFFER)
	for _,src := range this.from {
		this.ts[src] = time.Now()
	}
	go this.RecvFrom()
	go this.SendTo()
}

func (this *Heartbeat) SendTo() {
	ticker := time.NewTicker(time.Second * HEARTBEAT_TIMEOUT)
	for t := range ticker.C {
		// sendout
		for _, addr := range this.to {
			this.socket.WriteToUDP([]byte(this.host), addr)
			fmt.Println(this.host, "send heartbeat to", addr.String(), "at", t)
		}
		
		// check if dead
		for _, host := range this.from {
			if _, ok := this.ts[host]; !ok {
				fmt.Println("ERROR: this should never happen if Initialize correctly!")
			}
			if time.Now().After(this.ts[host].Add(time.Second * DEAD_TIMEOUT)) {
				fmt.Println("Dead Detected!", host)
				this.dead <- host
			}
		}
	}
}

func (this *Heartbeat) RecvFrom() {
	buf := make([]byte, 32)
	for {
		n, _, err := this.socket.ReadFromUDP(buf)
		if err != nil {
			fmt.Println("Error in heartbeat.RecvFrom: ",err)
		} 
		from := string(buf[0:n])
		fmt.Println(this.host, "receive heartbeat from", from)
		if _, ok := this.ts[from]; ok {
			this.ts[from] = time.Now()
			fmt.Println("update ts[", from, "] = ", this.ts[from])
		} else {
			fmt.Println("receive heartbeat from unknown host...")
		}
	}
}

func main() {
	master_addr := "127.0.0.1:10001"
	slave1_addr := "127.0.0.1:10002"
	slave2_addr := "127.0.0.1:10003"
	master_addrs := []string{master_addr}
	slave_addrs := []string{slave1_addr, slave2_addr}
	master_heartbeat := new(Heartbeat)
	master_heartbeat.Initialize(master_addr, slave_addrs, slave_addrs)
	slave1_heartbeat := new(Heartbeat)
	slave1_heartbeat.Initialize(slave1_addr, master_addrs, master_addrs)
	// slave2_heartbeat := new(Heartbeat)
	// slave2_heartbeat.Initialize(slave2_addr, master_addrs, master_addrs)
	deadChan := master_heartbeat.GetDeadChan()
	for {
		dead := <-deadChan
		fmt.Println(dead)
	}
}
