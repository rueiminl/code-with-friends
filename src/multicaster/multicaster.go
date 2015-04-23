package multicaster

import (
	"fmt"
	"net"
	"net/rpc"
	"time"
)

type PasserRPC struct {
	owner    *Multicaster
	ackChans map[string]chan string
}

type Multicaster struct {
	members      map[string]string //key: member id, value: ip:port
	port         string
	memberID     string
	passer       PasserRPC
	ackChans     map[string]chan string
	messageChans map[string]chan MessageInfo
	emChan       chan ElectionMsg
}

/*to recieve a Message from another node
* after recieving a Message from another node
* the node will send out ACK to other nodes
* and the node will hold back the message until
* it has recieved acks from all the other nodes(except itself and the sending node)
 */
func (this *PasserRPC) ReceiveMessage(message Message, reply *string) error {
	info := MessageInfo{}
	sName := message.Session
	if message.Type == "election" {
		*reply = "ack"
		this.owner.emChan <- message.Em
	} else if message.Type == "message" {
		*reply = "ack"
		memMap := this.owner.members
		for key := range memMap {
			//skip on sending message to itself and the sending node
			if key == "#" || key == message.Source {
				continue
			}

			addr := memMap[key]
			go this.owner.sendMessage(sName, addr, info, ElectionMsg{}, "ackR")
		}
		//fmt.Println("2")
		l := len(memMap)

		//wait for all the acks from other nodes
		for i := 0; i < l-2; i++ {
			<-this.ackChans[sName]
		}
		//deliver message
		this.owner.messageChans[sName] <- message.Content
		//send ack to the sending node to confirm that it has received the message
		go this.owner.sendMessage(sName, message.Source, info, ElectionMsg{}, "ackS")
		//fmt.Println("3")
	} else if message.Type == "ackR" {
		this.ackChans[sName] <- "ack"
	} else if message.Type == "ackS" {
		this.owner.ackChans[sName] <- "ack"
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
func (this *Multicaster) sendMessage(sName string, dest string, info MessageInfo, em ElectionMsg, mType string) string {
	c, err := rpc.Dial("tcp", dest)
	if err != nil {
		fmt.Println(err)
		return ""
	}
	var result string
	message := Message{this.members["#"], dest, info, em, mType, sName}
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
	//this.members["#"] = "127.0.0.1:" + newPort
	this.passer.ackChans = make(map[string]chan string)
	this.ackChans = make(map[string]chan string)
	this.messageChans = make(map[string]chan MessageInfo)
	this.emChan = make(chan ElectionMsg, 1024)
	this.members = make(map[string]string)
	this.members["#"] = "127.0.0.1:" + this.port
	go this.portListenner(":" + this.port)
}

/*
* add a member to the map,
* key is it's member id in string
* value is it's ip:port
 */
func (this *Multicaster) AddMember(memID, value string) {
	this.members[memID] = value
}

func (this *Multicaster) AddSession(sName string) {
	this.ackChans[sName] = make(chan string, 1024)
	this.messageChans[sName] = make(chan MessageInfo, 1024)
	this.passer.owner = this
	this.passer.ackChans[sName] = make(chan string, 1024)
	//fmt.Println(len(this.sessionMembers["test"]))
}

/*
* return the message mailbox
 */
func (this *Multicaster) GetMessageChan(sName string) chan MessageInfo {
	return this.messageChans[sName]
}

func (this *Multicaster) GetEmChan() chan ElectionMsg {
	return this.emChan
}

/*
* the blocking reliable multicast
* timeout arguement specifies how many seconds Multicaster will wait before it
* thinks the deliver fails
* if the deliver succeeds, it will return true, else it will return false
 */
func (this *Multicaster) Multicast(sName string, info MessageInfo, timeout int) bool {
	fmt.Println("len:")
	fmt.Println(len(this.members))
	for key := range this.members {
		//skip on sending message to itself
		if key == "#" {
			continue
		}

		addr := this.members[key]
		go this.sendMessage(sName, addr, info, ElectionMsg{}, "message")
	}

	l := len(this.members)
	for i := 0; i < l-1; i++ {
		select {
		case <-this.ackChans[sName]:
			//fmt.Println("get one")
		//time out
		case <-time.After(time.Second * time.Duration(timeout)):
			//fmt.Println("timeout")
			return false
		}
	}
	return true
}

func (this *Multicaster) SendElectionMessage(memID string, em ElectionMsg) {
	this.sendMessage("", this.members[memID], MessageInfo{}, em, "election")
}
