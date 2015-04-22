package multicaster

type Message struct {
	Source  string
	Dest    string
	Content MessageInfo
	Type    string
	Session string
}

type MessageInfo struct {
	SessionName   string
	CodeToExecute string
	MasterId      int
}
