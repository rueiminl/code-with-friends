

func Inititialize(...) (mc *MulticastType)


// Blocks until everything is sent
// Must be reliable
func (mc *MulticastType) Send(msg interface{})


func (mc *MulticastType) Receive() (msg interface{})

func (mc *MulticastType) getRecieveChannel() (chan interface{})
	return mc.channel
