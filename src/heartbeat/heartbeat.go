package heartbeat

import (
    "fmt"
	"net"
	"os"
	"time"
	"sync"
)

const (
	DEAD_TIMEOUT = 5
	HEARTBEAT_TIMEOUT = 1
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
	this.mutex = new(sync.Mutex)
	this.dead = make(chan string, DEAD_BUFFER)
	this.Update(from, to)
	go this.RecvFrom()
	go this.SendTo()
}

func (this *Heartbeat) Update(from []string, to []string) {
	this.mutex.Lock()
	this.from = from
	this.to = make([]*net.UDPAddr, len(to))
	for i, host := range to {
		addr, err := net.ResolveUDPAddr("udp", host)
		CheckError(err)
		this.to[i] = addr
	}
	this.ts = make(map[string]time.Time)
	this.mutex.Unlock()
}

func (this *Heartbeat) SendTo() {
	ticker := time.NewTicker(time.Second * HEARTBEAT_TIMEOUT)
	for _ = range ticker.C {
		// sendout
		this.mutex.Lock()
		for _, addr := range this.to {
			this.socket.WriteToUDP([]byte(this.host), addr)
			// fmt.Println(this.host, "send heartbeat to", addr.String())
		}
		
		// check if dead
		for from, ts := range this.ts {
			if time.Now().After(ts.Add(time.Second * DEAD_TIMEOUT)) {
				fmt.Println("Dead Detected!", from)
				delete(this.ts, from)
				this.dead <- from
			}
		}
		this.mutex.Unlock()
	}
}

func (this *Heartbeat) RecvFrom() {
	buf := make([]byte, 32)
	for {
		n, _, err := this.socket.ReadFromUDP(buf)
		if err != nil {
			// fmt.Println("Error in heartbeat.RecvFrom: ",err)
		} 
		from := string(buf[0:n])
		// fmt.Println(this.host, "receive heartbeat from", from)
		this.mutex.Lock()
		this.ts[from] = time.Now()
		// fmt.Println("update ts[", from, "] = ", this.ts[from])
		this.mutex.Unlock()
	}
}

func sampleTest() {
	master_addr := "127.0.0.1:10001"
	slave1_addr := "127.0.0.1:10002"
	slave2_addr := "127.0.0.1:10003"
		
	master_addrs := []string{master_addr}
	slave_addrs := []string{slave1_addr, slave2_addr}
	master_heartbeat := new(Heartbeat)
	master_heartbeat.Initialize(master_addr, slave_addrs, slave_addrs)
	slave1_heartbeat := new(Heartbeat)
	slave1_heartbeat.Initialize(slave1_addr, master_addrs, master_addrs)
	// remove slave2_heartbeat to test dead function
	// slave2_heartbeat := new(Heartbeat)
	// slave2_heartbeat.Initialize(slave2_addr, master_addrs, master_addrs)

	// need receive at least one notification (packet) to start detection
	time.Sleep(time.Second * 3)
	master_udpaddr, err := net.ResolveUDPAddr("udp", master_addr)
	CheckError(err)
	slave2_udpaddr, err := net.ResolveUDPAddr("udp", slave2_addr)
	CheckError(err)
	socket2, err := net.ListenUDP("udp", slave2_udpaddr)
	CheckError(err)
	socket2.WriteToUDP([]byte(slave2_addr), master_udpaddr)
	
	deadChan := master_heartbeat.GetDeadChan()
	for {
		dead := <-deadChan
		fmt.Println(dead)
		updated_slave_addrs := []string{slave1_addr}
		master_heartbeat.Update(updated_slave_addrs, updated_slave_addrs)
	}
}
