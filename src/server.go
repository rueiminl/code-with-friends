package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"heartbeat"
	"html/template"
	"io"
	"log"
	"masterselection"
	"multicaster"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

/*
Message returned from master, detailing information about a requested ioNumber.
The response will only include a valid requestedIo string if the currentIƒoNumber
is greater than the requested io number.
*/
type ioNumberResponse struct {
	currentIoNumber int
	requestedIo     string
}

/*
The message sent to the master from the goroutines handling STDOUT and STDERR.
They detail the origin of the output, and the output itself, so the master can save
it to a log (or send it to replicas).
*/
type ioMapWriteRequest struct {
	stdout bool   // True if stdout, else stdout.
	output string // Output from running process
}

type userInfo struct {
	userCode string // Code currently being typed by user.
}

/*
Details the information contained in a single python session, shared by a small group
of users. Also contains all necessary channels for interacting with the master of
the session.
*/
type PythonSession struct {
	sessionName         string
	inPipe              io.WriteCloser
	outPipe             io.ReadCloser
	errPipe             io.ReadCloser
	cmd                 *exec.Cmd
	ioNumber            int                     // Current index into ioMap. Starts at zero.
	userMap             map[string]*userInfo    // Map of all users.
	ioMap               map[int]string          // Map of all input/output/errors
	chMasterReady       chan bool               // OUTPUT : Written to by master when ready to execute.
	chExecuteCode       chan string             // INPUT : Channel of "please execute this code"
	chRequestUsername   chan string             // INPUT : "Does this user exist?"
	chUsernameExists    chan bool               // OUTPUT : Reponse from master, "user exists"
	chSetUsername       chan string             // INPUT : Add this user to the session.
	chRequestIoNumber   chan int                // INPUT : A goroutine is requesting the value of a particular io number.
	chResponseIoNumber  chan *ioNumberResponse  // OUTPUT : Response from master regarding requested io number.
	chIoMapWriteRequest chan *ioMapWriteRequest // INPUT : Request to write to ioMap.
	multicastMutex      *sync.Mutex             // Locks access to master's multicaster.
}

type Configuration struct {
	Servers []struct {
		Name      string `json:"name"`
		IP        string `json:"ip"`
		Port      string `json:"port"`
		HttpPort  string `json:"httpport"`
		Group     string `json:"group"`
		Heartbeat string `json:"heartbeat"`
	} `json:"servers"`
	Groups []struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	} `json:"groups"`
}

/*
Collection of global variables used by server.
*/
var (
	addr             = flag.Bool("addr", false, "find open address and print to final-port.txt")
	gopath           = os.Getenv("GOPATH")
	webpagesDir      = gopath + "webpages/"
	validPath        = regexp.MustCompile("^/(readactiveusers|readpartnercode|readsessionactive|readexecutedcode|executecode|edit|resetsession|joinsession)/([a-zA-Z0-9]*)$")
	sessionMap       = make(map[string]*PythonSession)
	configuration    = new(Configuration)
	serverId         = -1
	masterId         = -1
	groupId          = -1
	caster           = multicaster.Multicaster{}
	mutex            = &sync.Mutex{}
	mapElection      = make(map[int]int)
	heartbeatManager = new(heartbeat.Heartbeat)
)

/*
Function which renders the HTML page requested.
*/
func renderTemplate(w http.ResponseWriter, tmpl string) {
	t, err := template.ParseFiles(webpagesDir + tmpl + ".html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = t.Execute(w, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

/*
 * The session master is responsible for handling all requests relating
 * to a particular session. This avoids race conditions -- to update the
 * session, a request is made to the master, which serializes all operations
 * on the request rather than locking.
 *
 * The master identifies that it is ready to operate on a new request by
 * waiting on the channel "chMasterReady". This channel will be closed
 * when the master is no longer operational. Thus, from the perspective of
 * a goroutine interacting with the master, a handshake would look like the
 * following:
 *
 * MASTER : chMasterReady <- true
 * HELPER : _, ok := <- chMasterReady
 *   The helper can check if the master is still running here.
 *   If the master has not closed 'chMasterReady', the helper can assume
 *   the master is still serving requests.
 * HELPER : chMasterUpdateChannel <- request
 * MASTER : request, ok := <- chMasterUpdateChannel
 *   Master can serially modify state here.
 *
 */
func sessionMaster(s *PythonSession) {
	for {
		s.chMasterReady <- true // Only return true when s.cmd != nil.
		select {
		// Set a new user in this session.
		case uname, ok := <-s.chSetUsername:
			if !ok {
				fmt.Println("Closed chSetUsername")
			} else {
				fmt.Println("[SMASTER] New user in session: ", uname)
				s.userMap[uname] = &userInfo{}
			}
		// Does the user already exist in this session?
		case uname, ok := <-s.chRequestUsername:
			if !ok {
				fmt.Println("Closed chRequestUsername")
			} else {
				fmt.Println("[SMASTER] Requesting username: ", uname)
				if _, ok := s.userMap[uname]; ok {
					fmt.Println("exists")
					s.chUsernameExists <- true
				} else {
					fmt.Println("doesn't exist")
					s.chUsernameExists <- false
				}
			}
		// Please execute this code
		case inp, ok := <-s.chExecuteCode:
			if !ok {
				fmt.Println("Closed chExecuteCode.")
			} else {
				// TODO Add a buffered channel
				// TODO: Can this be done asynchronously?
				// Right now, Master will be blocked on code which takes a while to execute.
				fmt.Println("[SMASTER] Executing code")
				writeToSession(inp, s)
				s.ioMap[s.ioNumber] = "INP:" + inp
				s.ioNumber++
			}
		case request, ok := <-s.chRequestIoNumber:
			if !ok {
				fmt.Println("Closed chRequestIoNumber")
			} else {
				fmt.Println("[SMASTER] Responding to IO request")
				ioInfo := new(ioNumberResponse)
				ioInfo.currentIoNumber = s.ioNumber
				if ioInfo.currentIoNumber > request {
					ioInfo.requestedIo = s.ioMap[request]
				}
				s.chResponseIoNumber <- ioInfo
			}
		case request, ok := <-s.chIoMapWriteRequest:
			if !ok {
				fmt.Println("Closed chIoMapWriteRequest")
			} else {
				fmt.Println("[SMASTER] Responding to IO WRITE request")
				if request.stdout {
					s.ioMap[s.ioNumber] = "OUT:" + request.output
				} else { // stderr
					s.ioMap[s.ioNumber] = "ERR:" + request.output

				}
				s.ioNumber++
			}
		}
	}
}

func makeHandler(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Request incoming for: " + r.URL.Path)
		m := validPath.FindStringSubmatch(r.URL.Path)
		if m == nil && r.URL.Path != "/" {
			fmt.Println("Not found.")
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fn(w, r)
	}
}
func homeHandler(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "edit")
}
func editHandler(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "edit")
}

func redirectToCorrectSession(sessionName string, w http.ResponseWriter, r *http.Request) bool {
	desiredGroupId := getGroupId(sessionName)
	desiredGroup := configuration.Groups[desiredGroupId]

	if desiredGroupId != groupId {
		// Redirect session request to the appropriate server.
		fmt.Println("Redirecting request to the appropriate group: ", desiredGroupId)
		for _, s := range configuration.Servers {
			if s.Group == desiredGroup.Name {
				newURL := "https://" + s.IP + ":" + s.HttpPort // + r.URL.Path
				fmt.Println("\tRedirecting to ", newURL)
				fmt.Fprintf(w, newURL)
				return true
			}
		}
		// Ideally, we should have redirected already, but this prevents falling
		// through and executing on the wrong group.
		return true
	}
	return false
}

func sendExecuteRequestToSessionMaster(session *PythonSession, codeToExecute string) {
	_, ok := <-session.chMasterReady
	if ok {
		fmt.Println("writing to active session.")
		session.chExecuteCode <- codeToExecute // Must request that master handle session.
	} else {
		fmt.Println("No session active.")
	}
}

func executecodeHandler(w http.ResponseWriter, r *http.Request) {
	sessionName := r.FormValue("sessionName")
	// Parse necessary code to be excuted...
	codeToExecute := r.FormValue("codeToExecute")
	codeToExecute += "\n" // Required to terminate command.
	if strings.Count(codeToExecute, "\n") > 0 {
		codeToExecute += "\n" // Double termination (possibly) required for multiline commands.
	}

	fmt.Println("Trying to execute: ", codeToExecute)

	session := sessionMap[sessionName]
	if session == nil {
		return
	}

	// ... and send that code to the master to be written to the session.
	if masterId == serverId { // (masterId == -1) should not happen
		multicastExecuteCode(session, codeToExecute, sessionName)
	} else {
		// Send the request to the master.
		s := configuration.Servers[masterId]
		newURL := "https://" + s.IP + ":" + s.HttpPort + r.URL.Path
		fmt.Println("\tRedirecting to ", newURL)
		http.Redirect(w, r, newURL, 307)
	}
}

// Helper function to multicast START SESSION from MASTER --> REPLICAS
func multicastStartSession(sessionName string) bool {
	session := sessionMap[sessionName]
	if session != nil {
		// Master: The session already exists -- we multicasted it earlier.
		fmt.Println("(master): session already exists")
		return true
	}
	mi := multicaster.MessageInfo{}
	mi.SessionName = "SESSION_CREATOR"
	mi.CodeToExecute = sessionName
	mi.MasterId = serverId
	fmt.Println("(master) : about to multicast StartSession: ", sessionName)
	// The session is nil -- no need to lock.
	if caster.Multicast("SESSION_CREATOR", mi, 5) {
		fmt.Println("(master) : multicast succeeded.")
		fmt.Println("(master) : creating new sesion: ", sessionName)
		session := sessionMap[sessionName]
		if session == nil {
			// Master: Create session if we multicasted successfully
			sessionMap[sessionName] = createSession(sessionName)
			return true
		} else {
			panic("The session was nil, but after multicasting, we made it?")
		}
	} else {
		fmt.Println("(master) : multicast StartSession FAILED")
	}
	return false
}

// Helper function to multicast TYPED CODE from MASTER --> REPLICAS
func multicastTypedCode(s *PythonSession, code, user, sessionName string) {
	if masterId != serverId {
		panic("Only master should multicast")
	}
	mi := multicaster.MessageInfo{}
	mi.SessionName = sessionName
	mi.UserName = user
	mi.TypedCode = code
	mi.MasterId = serverId
	s.multicastMutex.Lock()
	defer s.multicastMutex.Unlock()
	if caster.Multicast(sessionName, mi, 5) {
		fmt.Println("Multicast typed code to session SUCCESS")
		s.userMap[user].userCode = code
	} else {
		fmt.Println("Multicast typed code to session FAILURE")
	}
}

// Helper function to multicast EXECUTED CODE from MASTER --> REPLICAS
func multicastExecuteCode(s *PythonSession, code, sessionName string) {
	if masterId != serverId {
		panic("Only master should multicast")
	}
	mi := multicaster.MessageInfo{}
	mi.SessionName = sessionName
	mi.CodeToExecute = code
	mi.MasterId = serverId
	s.multicastMutex.Lock()
	defer s.multicastMutex.Unlock()
	if caster.Multicast(sessionName, mi, 5) {
		fmt.Println("Multicast code to session SUCCESS")
		// The master can only apply the request locally if
		// all servers have applied it as well.
		sendExecuteRequestToSessionMaster(s, code)
	} else {
		fmt.Println("Multicast code to session FAILURE")
	}
}

// Helper function to multicast USERNAME from MASTER --> REPLICAS.
// Returns "true" on success.
func multicastUser(s *PythonSession, user, sessionName string) bool {
	if masterId != serverId {
		panic("Only master should multicast")
	}
	mi := multicaster.MessageInfo{}
	mi.SessionName = sessionName
	mi.UserName = user
	mi.MasterId = serverId
	s.multicastMutex.Lock()
	defer s.multicastMutex.Unlock()
	if caster.Multicast(sessionName, mi, 5) {
		fmt.Println("Multicast username to session SUCCESS")
		// The master can only add the user if all other servers
		// have added the user as well.
		return addUserToSession(s, user)
	} else {
		fmt.Println("Multicast username to session FAILURE")
		return false
	}
}

func readexecutedcodeHandler(w http.ResponseWriter, r *http.Request) {
	sessionName := r.FormValue("sessionName")

	urlPrefixLen := len("/readexecutedcode/")
	requestedIoNumber, err := strconv.Atoi(r.URL.Path[urlPrefixLen:])
	if err != nil {
		fmt.Println(err)
		return
	}

	session := sessionMap[sessionName]
	if session == nil {
		return
	}
	_, ok := <-session.chMasterReady
	if ok {
		session.chRequestIoNumber <- requestedIoNumber
		ioInfo := <-session.chResponseIoNumber

		if ioInfo.currentIoNumber > requestedIoNumber { // Client is catching up...
			fmt.Println("Result: " + ioInfo.requestedIo)
			fmt.Fprintf(w, ioInfo.requestedIo)
		} else if ioInfo.currentIoNumber < requestedIoNumber { // Client is ahead?
			fmt.Println("Client is ahead?")
			fmt.Fprintf(w, "ZERO")
		} else { // Client should wait.
			fmt.Fprintf(w, "")
		}

	}
}
func readsessionactiveHandler(w http.ResponseWriter, r *http.Request) {
	sessionName := r.FormValue("sessionName")
	session := sessionMap[sessionName]
	if session != nil && session.cmd != nil {
		fmt.Fprintf(w, "ACTIVE")
	} else {
		fmt.Fprintf(w, "DEAD")
	}
}

func readactiveusersHandler(w http.ResponseWriter, r *http.Request) {
	sessionName := r.FormValue("sessionName")
	session := sessionMap[sessionName]
	if session != nil && session.cmd != nil {
		users := []string{}
		for user := range session.userMap {
			users = append(users, user)
		}
		sort.Strings(users)
		fmt.Fprintf(w, strings.Join(users, "\n"))
	} else {
		fmt.Fprintf(w, "")
	}
}

/*
Sets AND gets user + partner code.
TODO: Multicast user code between all servers.
*/
func readpartnercodeHandler(w http.ResponseWriter, r *http.Request) {
	sessionName := r.FormValue("sessionName")
	// This is the user calling, giving us information about their code.
	userName := r.FormValue("userName")
	userCode := r.FormValue("userCode")
	// This is the partner, that the user wants information about.
	partnerName := r.FormValue("partnerName")

	// TODO SECURITY.
	session := sessionMap[sessionName]
	// If the session exists...
	if session != nil && session.cmd != nil {
		if masterId == serverId { // And we're the master...
			multicastTypedCode(session, userCode, userName, sessionName)
			// Regardless of multicast, return partner code...
			userInfo := session.userMap[partnerName]
			if userInfo == nil {
				fmt.Fprintf(w, "")
			} else {
				fmt.Fprintf(w, userInfo.userCode)
			}
		} else { // And we're a replica...
			// Send the request to the master.
			s := configuration.Servers[masterId]
			newURL := "https://" + s.IP + ":" + s.HttpPort + r.URL.Path
			fmt.Println("\tRedirecting to ", newURL)
			http.Redirect(w, r, newURL, 307)
		}
	}
}

/*
TODO FIXME
*/
func resetsessionHandler(w http.ResponseWriter, r *http.Request) {
	// argv := []string{"-i"}
	sessionName := r.FormValue("sessionName")
	fmt.Println("RESET SESSION" + sessionName)
	session := sessionMap[sessionName]
	if session == nil {
		session = new(PythonSession)
	}
	if session.cmd != nil {
		session.cmd.Process.Kill()
		fmt.Println("Tried to kill old session.")
	}
	argv := "-i"
	binary, err := exec.LookPath("python")
	session.cmd = exec.Command(binary, argv)
	if session.cmd == nil {
		fmt.Println("error: ")
		fmt.Println(err)
		return
	}
	session.ioNumber = 0
	session.ioMap = make(map[int]string)
	session.chMasterReady = make(chan bool)
	session.chExecuteCode = make(chan string)
	session.chRequestUsername = make(chan string)
	session.chUsernameExists = make(chan bool)
	session.chSetUsername = make(chan string)
	session.chRequestIoNumber = make(chan int)
	session.chResponseIoNumber = make(chan *ioNumberResponse)
	session.chIoMapWriteRequest = make(chan *ioMapWriteRequest)
	session.multicastMutex = &sync.Mutex{}

	sessionMap[sessionName] = session
	fmt.Println("Created a new process: ")
	session.inPipe, err = session.cmd.StdinPipe()
	if err != nil {
		fmt.Println("Cannot make stdin pipe")
		return
	}
	session.outPipe, err = session.cmd.StdoutPipe()
	if err != nil {
		fmt.Println("Cannot make stdout pipe")
		return
	}
	session.errPipe, err = session.cmd.StderrPipe()
	if err != nil {
		fmt.Println("Cannot make stderr pipe")
		return
	}
	go handleSessionOutput(session) // Start listening to STDOUT/STDERR.
	err = session.cmd.Start()
	if err != nil {
		fmt.Println("Start cannot run")
		fmt.Println(err)
		return
	}
	//fmt.Println("About to do print hello world command")
	//writeToSession("print 'hello world'\n", session)
	//fmt.Println("about to wait...")
	go waitForSessionDeath(session)
	go sessionMaster(session)
}

/*
Function which creates a new LOCAL python session.
TODO Sandbox
*/
func createSession(name string) *PythonSession {
	session := new(PythonSession)
	session.sessionName = name
	argv := "-i"
	binary, err := exec.LookPath("python")
	session.cmd = exec.Command(binary, argv)
	if session.cmd == nil {
		fmt.Println("error: ")
		fmt.Println(err)
		return nil
	}
	session.ioNumber = 0
	session.ioMap = make(map[int]string)
	session.chMasterReady = make(chan bool)
	session.chExecuteCode = make(chan string)
	session.chRequestUsername = make(chan string)
	session.chUsernameExists = make(chan bool)
	session.chSetUsername = make(chan string)
	session.chRequestIoNumber = make(chan int)
	session.chResponseIoNumber = make(chan *ioNumberResponse)
	session.chIoMapWriteRequest = make(chan *ioMapWriteRequest)
	session.multicastMutex = &sync.Mutex{}
	caster.AddSession(session.sessionName)
	session.userMap = make(map[string]*userInfo)

	fmt.Println("Created a new process: ")
	session.inPipe, err = session.cmd.StdinPipe()
	if err != nil {
		fmt.Println("Cannot make stdin pipe")
		return nil
	}
	session.outPipe, err = session.cmd.StdoutPipe()
	if err != nil {
		fmt.Println("Cannot make stdout pipe")
		return nil
	}
	session.errPipe, err = session.cmd.StderrPipe()
	if err != nil {
		fmt.Println("Cannot make stderr pipe")
		return nil
	}
	go handleSessionOutput(session) // Start listening to STDOUT/STDERR.
	err = session.cmd.Start()
	if err != nil {
		fmt.Println("Start cannot run")
		fmt.Println(err)
		return nil
	}
	//fmt.Println("About to do print hello world command")
	//writeToSession("print 'hello world'\n", session)
	//fmt.Println("about to wait...")
	go waitForSessionDeath(session)
	go sessionMaster(session)
	go receiveMulticast(session.sessionName)
	return session
}

/*
Lets users join a session
*/
func joinsessionHandler(w http.ResponseWriter, r *http.Request) {
	sessionName := r.FormValue("newSessionName")
	sessionName = strings.Trim(sessionName, " ")
	if sessionName == "" || sessionName[0] == '#' {
		fmt.Fprintf(w, "SNAMEFAILURE")
		return
	}
	if redirectToCorrectSession(sessionName, w, r) {
		return
	}
	newCoderName := r.FormValue("newCoderName")
	// "\n" used as separator when returning list of users later.
	newCoderName = strings.Replace(strings.Trim(newCoderName, " "),
		"\n", " ", -1)
	if newCoderName == "" {
		fmt.Fprintf(w, "UNAMEFAILURE")
		return
	}
	fmt.Println("JOIN SESSION " + sessionName)
	if masterId == serverId {
		fmt.Println("(master) creating session")
		// Master case: create the session.
		if multicastStartSession(sessionName) {
			fmt.Println("(master) : created session")
			session := sessionMap[sessionName]
			// Master case: add the user.
			if multicastUser(session, newCoderName, sessionName) {
				fmt.Println("(master) : Adding user to session: ", newCoderName)
				fmt.Fprintf(w, "SUCCESS")
			}
		}
	} else if masterId != serverId {
		// Send the request to the master.
		s := configuration.Servers[masterId]
		newURL := "https://" + s.IP + ":" + s.HttpPort + r.URL.Path
		fmt.Println("\tRedirecting to ", newURL)
		http.Redirect(w, r, newURL, http.StatusTemporaryRedirect)
		return
	}
	// Failure case
	fmt.Fprintf(w, "UNAMEFAILURE")
}

/*
Helper which communicates with master to add a user to
a python session.

Returns "true" on success, "false" on failure.
*/
func addUserToSession(session *PythonSession, newCoderName string) bool {
	_, ok := <-session.chMasterReady
	if ok {
		session.chRequestUsername <- newCoderName
		exists := <-session.chUsernameExists
		if exists {
			// User already exists here.
			return false
		} else {
			// TODO (master)  Multicast list of active users.
			<-session.chMasterReady
			session.chSetUsername <- newCoderName
			// TODO Should there be a timeout / removal mechanism for inactive users?
			return true
		}
	} else {
		fmt.Println("Master NOT ready")
		return false
	}
}

func waitForSessionDeath(s *PythonSession) {
	s.cmd.Wait()
	// TODO Possibly handle closing channels? We don't want any stuck goroutines.
	// TODO Remove global refs (garbage collecting)
}

func handleSessionOutput(s *PythonSession) {
	outPipe := s.outPipe
	errPipe := s.errPipe
	go handlePythonSingleOutput(outPipe, s, true)
	go handlePythonSingleOutput(errPipe, s, false)
}

/*
Function which continuously reads input from STDOUT/STDERR and propagates
this information to the master.
*/
func handlePythonSingleOutput(outPipe io.ReadCloser, s *PythonSession, stdout bool) {
	output := make([]byte, 1000) // TODO Maybe change this from 1000 to something bigger?
	for {
		n, err := outPipe.Read(output)
		if err != nil {
			fmt.Println("(out/err) ERROR")
			fmt.Println(err)
			// TODO Is there any other work which needs to be done to shut down the session?
			return
		} else {
			fmt.Println("(out/err) saw: " + string(output[:n]))
			_, ok := <-s.chMasterReady // Master is ready for our output!
			if ok {
				request := new(ioMapWriteRequest)
				request.stdout = stdout
				request.output = string(output[:n])
				s.chIoMapWriteRequest <- request
			}
		}
	}
}

/*
Low-level function used by master to communicate with active python session.
All executed code is sent through here.
*/
func writeToSession(inputString string, s *PythonSession) {
	inPipe := s.inPipe
	input := []byte(inputString)
	n, err := inPipe.Write(input)
	fmt.Printf("(in) Wrote %d bytes: %s\n", n, string(input[:n]))
	if err != nil {
		fmt.Println("(in) ERROR")
		fmt.Println(err)
	}
}

func getGroupId(sessionName string) int {
	h := fnv.New32a()
	h.Write([]byte(sessionName))
	return int(h.Sum32()) % len(configuration.Groups)
}

// debug only
func showConfiguration() {
	for i, server := range configuration.Servers {
		fmt.Printf("server[%d]: %s (%s:%s) listen on %s\n", i, server.Name, server.IP, server.Port, server.HttpPort)
	}
	for i, group := range configuration.Groups {
		fmt.Printf("group[%d] = %v\n", i, group)
	}
	fmt.Printf("I am %d-th server (%s) in %d-th Group (%s)\n", serverId, configuration.Servers[serverId].Name, groupId, configuration.Groups[groupId].Name)

}

// Function from which REPLICAS create a session.
func receiveMulticastSessionInitializer() {
	fmt.Println("receiveMulticastSessionInitializer")
	ch := caster.GetMessageChan("SESSION_CREATOR")
	for {
		mi := <-ch
		fmt.Println("(receive session initializer) New message: ", mi)
		sessionName := mi.CodeToExecute
		session := sessionMap[sessionName]
		if session == nil {
			// Replica: Create a session only here.
			sessionMap[sessionName] = createSession(sessionName)
		}
	}
}

// Goroutine for each session, listening to the multicaster.
func receiveMulticast(sessionName string) {
	ch := caster.GetMessageChan(sessionName)
	for {
		mi := <-ch
		// TODO FIXME: Use master election here!
		/* // should not happen
		if masterId == -1 {
			fmt.Printf("Multicast received: Setting master to %d\n", mi.MasterId)
			masterId = mi.MasterId
		}
		*/
		fmt.Println("New message: ", mi)
		sessionName := mi.SessionName
		session := sessionMap[sessionName]
		if session == nil {
			// This shouls never happen -- createServer must be called
			// before receiveMulticast.
			panic("Session must be created before receiving multicast")
		}
		codeToExecute := mi.CodeToExecute
		userName := mi.UserName
		typedCode := mi.TypedCode
		if typedCode != "" && userName != "" {
			// This message is from the master, updating us about this user's code.
			session.userMap[userName].userCode = typedCode
		} else if codeToExecute != "" {
			// This message is from the master, telling us to execute code.
			sendExecuteRequestToSessionMaster(session, codeToExecute)
		} else if userName != "" {
			if addUserToSession(session, userName) {
				fmt.Println("User added: ", userName)
			} else {
				fmt.Println("User could not be added: ", userName)
			}
		}
	}
}

func usage() {
	fmt.Printf("usage: %s serverName\n", os.Args[0])
}

func checkDead() {
	fmt.Println("checkDead")
	deadChan := heartbeatManager.GetDeadChan()
	for {
		dead := <-deadChan
		fmt.Println(dead)
		deadId := -1
		if serverId == masterId {
			fmt.Println("slave die")
			// master get a notification that a slave has been dead
			for i, server := range configuration.Servers {
				if dead == server.IP+":"+server.Heartbeat {
					deadId = i
					break
				}
			}
			caster.RemoveMemInGroup(strconv.Itoa(masterId))
			fmt.Println("UpdateLinkedMap")
			multicaster.UpdateLinkedMap(deadId, mapElection)
			// TODO notify slaves to UpdateLinkedMap
		} else {
			// slave get the notification that the master has been dead
			fmt.Println("master die")
			oldMaster := masterId
			caster.RemoveMemLocal(strconv.Itoa(masterId))
			fmt.Println("Are you QualifiedToRaise?")
			if masterelection.QualifiedToRaise(serverId, masterId, mapElection, &masterId) {
				fmt.Println("RaiseElection")
				masterelection.RaiseElection(serverId, caster, mapElection)
			}
			electionChan := caster.GetEmChan()
			fmt.Println("start loop")
			for {
					em := <-electionChan
					fmt.Println(em.NewMasterId)
					if masterelection.ReadElectionMsg(serverId, em, caster, mapElection, &masterId) {
						fmt.Println("Master settle, new master is: " + strconv.Itoa(masterId))
						break
					}
			}
			fmt.Println("oldMaster: " + strconv.Itoa(oldMaster))
			multicaster.UpdateLinkedMap(oldMaster, mapElection)
		}
	}
}

func main() {
	// server name should be the first argument
	if len(os.Args) < 2 {
		usage()
		os.Exit(0)
	}
	serverName := os.Args[1]

	// read configuration
	file, err := os.Open("conf.json")
	if err != nil {
		log.Fatal(err)
	}
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&configuration)
	if err != nil {
		log.Fatal(err)
	}

	// search serverName in configuration
	for i, server := range configuration.Servers {
		if serverName == server.Name {
			serverId = i
			break
		}
	}
	if serverId < 0 {
		log.Fatal("Error: server " + serverName + " not found in configuration")
		os.Exit(0)
	}

	// search group in configuration
	for i, group := range configuration.Groups {
		if configuration.Servers[serverId].Group == group.Name {
			groupId = i
			break
		}
	}
	if groupId < 0 {
		log.Fatal("Error: group " + configuration.Servers[serverId].Group + " not found in configuration")
		os.Exit(0)
	}

	// initialize mapElection masterId (max one in the group) by serverId and groupId
	first := -1
	masterId = -1
	// FIXME FIXME FIXME THIS IS A HACK, NOT AN ELECTION. LOWEST IN
	// CONFIG FILE BECOMES MASTER.
	for i, server := range configuration.Servers {
		if configuration.Groups[groupId].Name == server.Group {
			if first == -1 {
				first = i
			}
			if masterId != -1 {
				mapElection[masterId] = i
			}
			masterId = i
		}
	}
	mapElection[masterId] = first

	// initialize heartbeat
	if serverId == masterId {
		// master should monitor all slaves excluding itself
		slaves := make([]string, len(configuration.Servers)-1)
		i := 0
		for id, server := range configuration.Servers {
			if id == serverId {
				continue
			}
			if configuration.Groups[groupId].Name == server.Group {
				slaves[i] = server.IP + ":" + server.Heartbeat
				i++
			}
		}
		fmt.Println("slaves = ", slaves)
		heartbeatManager.Initialize(configuration.Servers[serverId].IP+":"+configuration.Servers[serverId].Heartbeat, slaves, slaves)
	} else {
		master := []string{configuration.Servers[masterId].IP + ":" + configuration.Servers[masterId].Heartbeat}
		fmt.Println("master = ", master)
		heartbeatManager.Initialize(configuration.Servers[serverId].IP+":"+configuration.Servers[serverId].Heartbeat, master, master)
	}

	// initialize multicast
	caster.Initialize(configuration.Servers[serverId].IP,
		configuration.Servers[serverId].Port)
	for i, server := range configuration.Servers {
		if serverId == i {
			continue
		}
		if configuration.Groups[groupId].Name == server.Group {
			caster.AddMember(strconv.Itoa(i), server.IP+":"+server.Port)
		}
	}
	caster.AddSession("SESSION_CREATOR")
	go receiveMulticastSessionInitializer()
	// debug only
	showConfiguration()

	go checkDead()

	flag.Parse()
	http.HandleFunc("/", makeHandler(homeHandler))
	http.HandleFunc("/edit/", makeHandler(editHandler))
	http.HandleFunc("/executecode/", makeHandler(executecodeHandler))
	http.HandleFunc("/readexecutedcode/", makeHandler(readexecutedcodeHandler))
	http.HandleFunc("/readsessionactive/", makeHandler(readsessionactiveHandler))
	http.HandleFunc("/readactiveusers/", makeHandler(readactiveusersHandler))
	http.HandleFunc("/readpartnercode/", makeHandler(readpartnercodeHandler))
	http.HandleFunc("/joinsession/", makeHandler(joinsessionHandler))
	http.HandleFunc("/resetsession/", makeHandler(resetsessionHandler))

	/*
		if *addr {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				log.Fatal(err)
			}
			err = ioutil.WriteFile("final-port.txt", []byte(l.Addr().String()), 0644)
			if err != nil {
				log.Fatal(err)
			}
			s := &http.Server{}
			s.Serve(l)
			return
		}*/

	ip := configuration.Servers[serverId].IP
	http.ListenAndServeTLS(":"+configuration.Servers[serverId].HttpPort,
		os.Getenv("GOPATH")+"keys/"+ip+".cert",
		os.Getenv("GOPATH")+"keys/"+ip+".key",
		nil)
}
