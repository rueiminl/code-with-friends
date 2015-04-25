package multicaster

type Message struct {
	Source   string
	Dest     string
	MemToDlt string
	Content  MessageInfo
	Em       ElectionMsg
	Type     string
	Session  string
}

type MessageInfo struct {
	SessionName   string
	UserName      string
	UserKey       string
	TypedCode     string
	CodeToExecute string
	MasterId      int
}

/*
ElectionMsg:
    map -> If slave id not in map, join the election by add into the map and set the value to 'false'.
    	   If slave id is in the map, and map[id] = false, set masterId = newMasterId
    	   If slave id is in the map and value equals to true, finish election.

   	newMasterId -> Initialize as -1, and after first round, pick the largest number in the map.
*/
type ElectionMsg struct {
	MasterSelectSet map[int]bool
	NewMasterId     int
}
