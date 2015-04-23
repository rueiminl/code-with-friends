package masterelection

import (
    "fmt"
    "strconv"
    "multicaster"
//	"net"
//	"os"
)

type ElectionMsg struct{
	MasterSelectSet map[int]bool
	NewMasterId int
}

/* 
If the master die(slaves will not receive heartbeat from master),
slaves will check if they're qualified to raise the master Election
*/
func QualifiedToRaise(id int, master int, m map[int]int, masterId *int) bool{
	if value, ok := m[id]; ok {
		if value == master {
			UpdateLinkedMap(master, m)
			if len(m) == 1 {
				*masterId = id
				fmt.Println("You are now the only one remain in the server")
				return false
			}
			for key, value := range m{
				fmt.Println("key: " + strconv.Itoa(key) + ", value: " + strconv.Itoa(value))
			}
			return true
		}
	}
	return false
}

/*
If the slave is qalified to raise an election, call this function
This function will initiallize the election message, and pass through the ring
*/
func RaiseElection(id int, caster multicaster.Multicaster, m map[int]int) {
	msg := multicaster.ElectionMsg{}
	msg.MasterSelectSet = make(map[int]bool)
	msg.NewMasterId = -1
	msg.MasterSelectSet[id] = false
	// TODO: pass message to the next element in the link
	caster.SendElectionMessage(strconv.Itoa(m[id]), msg)
}

/*

*/
func ReadElectionMsg(id int, msg multicaster.ElectionMsg, caster multicaster.Multicaster, m map[int]int, masterId *int) bool{
	if value, ok := msg.MasterSelectSet[id]; ok {
		if value == true {
			fmt.Println("Election finished")
			return true
			// Everyone knew who the master is, stop election.
		} else{
			msg.MasterSelectSet[id] = true
			if msg.NewMasterId == -1{
				for key, _ := range msg.MasterSelectSet {
					if msg.NewMasterId < key {
						msg.NewMasterId = key
					}
				}
			}
			*masterId = msg.NewMasterId
			caster.SendElectionMessage(strconv.Itoa(m[id]), msg)
		}
	} else{
		// First round election
		msg.MasterSelectSet[id] = false
		caster.SendElectionMessage(strconv.Itoa(m[id]), msg)
	}
	return false
}

/*
If some node die, update the linked map list.
*/
func UpdateLinkedMap(id int, m map[int]int){
	for key, value := range m {
		if value == id {
			m[key] = m[id]
			delete(m, id)
			break
		}
	}
}
/*
func main(){
	var m = make(map[int]int)
	m[6] = 5
	m[5] = 0
	m[0] = 1
	m[1] = 4
	m[4] = 3
	m[3] = 6
	// fmt.Println(qualifiedToRaise(3, 6, m))

}*/