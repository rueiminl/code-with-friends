package main

import (
    "fmt"
//	"net"
//	"os"
)

type ElectionMsg struct{
	masterSelectSet map[int]bool
	newMasterId int
}

/* 
If the master die(slaves will not receive heartbeat from master),
slaves will check if they're qualified to raise the master Election
*/
func qualifiedToRaise(id int, master int, m map[int]int) bool{
	if value, ok := m[id]; ok {
		if value == master {
			updateLinkedMap(master, m)
			if len(m) == 0 {
				masterId = id
				fmt.Println("You are now the only one remain in the server")
				return false
			}
			return true
		}
	}
	else{
		// Only one node remains in the group
		masterId = id
	}
	return false
}

/*
If the slave is qalified to raise an election, call this function
This function will initiallize the election message, and pass through the ring
*/
func raiseElection(id int) {
	msg := new(multicaster.ElectionMsg)
	msg.masterSelectSet = make(map[int]bool)
	msg.newMasterId = -1
	msg.masterSelectSet[id] = false
	// TODO: pass message to the next element in the link
	//caster.sendMessage()
}

/*

*/
func readElectionMsg(id int, msg multicaster.ElectionMsg){
	if value, ok := msg.masterSelectSet[id]; ok {
		if value == true {
			fmt.Println("Election finished")
			// Everyone knew who the master is, stop election.
		} else{
			msg.masterSelectSet[id] = true
			if msg.newMasterId == -1{
				for key, _ := range msg.masterSelectSet {
					if msg.newMasterId < key {
						msg.newMasterId = key
					}
				}
			}
			// TODO: Set our new master
			masterId = msg.newMasterId
			// TODO: Pass to next node

		}
	} else{
		// First round election
		msg.masterSelectSet[id] = false
		// TODO: send to next node(via linked map list)
	}
}

/*
If some node die, update the linked map list.
*/
func updateLinkedMap(id int, m map[int]int){
	for key, value := range m {
		if value == id {
			if len(m) != 1{
				m[key] = m[id]
				delete(m, id)
			} else {
				delete(m, key)
			}
			break
		}
	}
}

func main(){
	var m = make(map[int]int)
	m[6] = 5
	m[5] = 0
	m[0] = 1
	m[1] = 4
	m[4] = 3
	m[3] = 6
	fmt.Println(qualifiedToRaise(3, 6, m))

}