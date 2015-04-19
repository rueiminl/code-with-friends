package multicaster

import (
	"fmt"
	"net"
	"net/rpc"
	"time"
)

type PasserRPC struct {
	owner   *Multicaster
	ackChan chan string
}

type Multicaster struct {
	members     map[string]string //key: member id, value: ip:port
	port        string
	memberID    string
	passer      PasserRPC
	ackChan     chan string
	messageChan chan MessageInfo
}

/*to recieve a Message from another node
* after recieving a Message from another node
* the node will send out ACK to other nodes
* and the node will hold back the message until
* it has recieved acks from all the other nodes(except itself and the sending node)
 */
func (this *PasserRPC) ReceiveMessage(message Message, reply *string) error {
	info := &MessageInfo{"", "", 0}
	if message.Type == "message" {
		*reply = "ack"
		memMap := this.owner.members
		for key := range memMap {
			//skip on sending message to itself and the sending node
			if key == "#" || key == message.Source {
				continue
			}

			addr := memMap[key]
			go this.owner.sendMessage(addr, info, "ackR")
		}
		l := len(memMap)

		//wait for all the acks from other nodes
		for i := 0; i < l-2; i++ {
			<-this.ackChan
		}
		//deliver message
		this.owner.messageChan <- message.Content
		//send ack to the sending node to confirm that it has received the message
		go this.owner.sendMessage(message.Source, info, "ackS")
	} else if message.Type == "ackR" {
		this.ackChan <- "ack"
	} else if message.Type == "ackS" {
		this.owner.ackChan <- "ack"
	}
	return nil
}

func (this *Multicaster) portListenner(port string) {
	rpc.Register(&(this.passer))
	ln, err := net.Listen("tcp", port)
	if err != nil {
		fmt.Println(err)
		return
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			// handle error
			fmt.Println(err)
		}
		go rpc.ServeConn(conn)
	}
}

/*
* send out the message to a single node,
* the return value is the reply from the dest node
 */
func (this *Multicaster) sendMessage(dest string, info *MessageInfo, mType string) string {
	c, err := rpc.Dial("tcp", dest)
	if err != nil {
		fmt.Println(err)
		return ""
	}
	var result string
	message := &Message{this.members["#"], dest, *info, mType}
	err = c.Call("PasserRPC.ReceiveMessage", message, &result)
	//fmt.Println(result)
	if err != nil {
		fmt.Println(err)
	}
	return result
}

/*
* this function should be called before multicaster being used
 */
func (this *Multicaster) Initialize(newPort string) {
	this.members = make(map[string]string)
	this.port = newPort
	this.members["#"] = "127.0.0.1:" + newPort
	this.passer.owner = this
	this.passer.ackChan = make(chan string, 1024)
	this.ackChan = make(chan string, 1024)
	this.messageChan = make(chan MessageInfo, 1024)
	go this.portListenner(":" + this.port)
}

/*
* add a member to the map,
* key is it's member id in string
* value is it's ip:port
 */
func (this *Multicaster) AddMember(key, value string) {
	this.members[key] = value
}

/*
* return the message mailbox
 */
func (this *Multicaster) GetMessageChan() chan MessageInfo {
	return this.messageChan
}

/*
* the blocking reliable multicast
* timeout arguement specifies how many seconds Multicaster will wait before it
* thinks the deliver fails
* if the deliver succeeds, it will return true, else it will return false
 */
func (this *Multicaster) Multicast(info *MessageInfo, timeout int) bool {
	for key := range this.members {
		//skip on sending message to itself
		if key == "#" {
			continue
		}

		addr := this.members[key]
		go this.sendMessage(addr, info, "message")
	}

	l := len(this.members)
	for i := 0; i < l-1; i++ {
		select {
		case <-this.ackChan:
		//time out
		case <-time.After(time.Second * time.Duration(timeout)):
			return false
		}
	}
	return true
}
