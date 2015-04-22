package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"multicaster"
	"net"
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
The response will only include a valid requestedIo string if the currentIoNumber
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
	inPipe              io.WriteCloser
	outPipe             io.ReadCloser
	errPipe             io.ReadCloser
	cmd                 *exec.Cmd
	ioNumber            int                     // Current index into ioMap. Starts at zero.
	userMap             map[string]*userInfo    // Map of all users.
	ioMap               map[int]string          // Map of all input/output/errors
	chMasterReady       chan bool               // OUTPUT : Written to by master when ready to execute.
	chExecuteCode       chan string             // INPUT : Channel of "please execute this code"
	chRequestIoNumber   chan int                // INPUT : A goroutine is requesting the value of a particular io number.
	chResponseIoNumber  chan *ioNumberResponse  // OUTPUT : Response from master regarding requested io number.
	chIoMapWriteRequest chan *ioMapWriteRequest // INPUT : Request to write to ioMap.
}

type Configuration struct {
	Servers []struct {
		Name     string `json:"name"`
		IP       string `json:"ip"`
		Port     string `json:"port"`
		HttpPort string `json:"httpport"`
		Group    string `json:"group"`
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
	addr          = flag.Bool("addr", false, "find open address and print to final-port.txt")
	gopath        = os.Getenv("GOPATH")
	webpagesDir   = gopath + "webpages/"
	validPath     = regexp.MustCompile("^/(readactiveusers|readpartnercode|readsessionactive|readexecutedcode|executecode|edit|resetsession|joinsession)/([a-zA-Z0-9]*)$")
	sessionMap    = make(map[string]*PythonSession)
	configuration = new(Configuration)
	serverId      = -1
	masterId      = -1
	groupId       = -1 // TODO SET FROM CONF. REDIRECT.
	caster        = new(multicaster.Multicaster)
	mutex         = &sync.Mutex{}
	mapElection   = make(map[int]int)
)

/*
ElectionMsg:
    map -> If slave id not in map, join the election by add into the map and set the value to 'false'.
    	   If slave id is in the map, and map[id] = false, set masterId = newMasterId
    	   If slave id is in the map and value equals to true, finish election.

   	newMasterId -> Initialize as -1, and after first round, pick the largest number in the map.
*/
type ElectionMsg struct {
	masterSelectSet map[int]bool
	newMasterId     int
}

/*
If the master die(slaves will not receive heartbeat from master),
slaves will check if they're qualified to raise the master Election
*/
func qualifiedToRaise(id int, master int, m map[int]int) bool {
	if value, ok := m[id]; ok {
		if value == master {
			updateLinkedMap(master, m)
			raiseElection(id)
			return true
		}
	}
	return false
}

/*
If the slave is qalified to raise an election, call this function
This function will initiallize the election message, and pass through the ring
*/
func raiseElection(id int) {
	msg := new(ElectionMsg)
	msg.masterSelectSet = make(map[int]bool)
	msg.newMasterId = -1
	msg.masterSelectSet[id] = false
	// TODO: pass message to the next element in the link
}

/*

*/
func readElectionMsg(id int, msg ElectionMsg) {
	if value, ok := msg.masterSelectSet[id]; ok {
		if value == true {
			fmt.Println("Election finished")
			// Everyone knew who the master is, stop election.
		} else {
			msg.masterSelectSet[id] = true
			if msg.newMasterId == -1 {
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
	} else {
		// First round election
		msg.masterSelectSet[id] = false
		// TODO: send to next node(via linked map list)
	}
}

/*
If some node die, update the linked map list.
*/
func updateLinkedMap(id int, m map[int]int) {
	for key, value := range m {
		if value == id {
			m[key] = m[id]
			delete(m, id)
			break
		}
	}
}

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
		case inp, ok := <-s.chExecuteCode:
			if !ok {
				fmt.Println("Closed chExecuteCode.")
			} else {
				// TODO: Can this be done asynchronously?
				// Right now, Master will be blocked on code which takes a while to execute.
				writeToSession(inp, s)
				s.ioMap[s.ioNumber] = "INP:" + inp
				s.ioNumber++
			}
		case request, ok := <-s.chRequestIoNumber:
			if !ok {
				fmt.Println("Closed chRequestIoNumber")
			} else {
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
				newURL := "http://" + s.IP + ":" + s.HttpPort // + r.URL.Path
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
	fmt.Println("About to ask if master is ready")
	_, ok := <-session.chMasterReady
	fmt.Println("Got OK from master session")
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
	if (masterId == -1) || (masterId == serverId) {
		// In this case, we ARE the master.
		mi := multicaster.MessageInfo{sessionName, codeToExecute, serverId}
		mutex.Lock()
		if caster.Multicast(sessionName, mi, 5) {
			fmt.Println("Multicast code to session SUCCESS")
			masterId = serverId
			sendExecuteRequestToSessionMaster(session, codeToExecute)
		} else {
			fmt.Println("Multicast code to session FAILURE")
		}
		mutex.Unlock()
	} else {
		// Send the request to the master.
		s := configuration.Servers[masterId]
		newURL := "http://" + s.IP + ":" + s.HttpPort + r.URL.Path
		fmt.Println("\tRedirecting to ", newURL)
		http.Redirect(w, r, newURL, 307)
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

	// TODO WHEN MULTIPLE SESSIONS EXIST Lookup the sesion here.
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
	userName := r.FormValue("userName")
	userCode := r.FormValue("userCode")
	partnerName := r.FormValue("partnerName")

	// TODO SECURITY.
	session := sessionMap[sessionName]
	if session != nil && session.cmd != nil {
		session.userMap[userName].userCode = userCode
		userInfo := session.userMap[partnerName]
		if userInfo == nil {
			fmt.Fprintf(w, "")
		} else {
			fmt.Fprintf(w, userInfo.userCode)
		}
	}
}

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
	session.chRequestIoNumber = make(chan int)
	session.chResponseIoNumber = make(chan *ioNumberResponse)
	session.chIoMapWriteRequest = make(chan *ioMapWriteRequest)

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

func createSession() *PythonSession {
	session := new(PythonSession)
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
	session.chRequestIoNumber = make(chan int)
	session.chResponseIoNumber = make(chan *ioNumberResponse)
	session.chIoMapWriteRequest = make(chan *ioMapWriteRequest)
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
	return session
}

func joinsessionHandler(w http.ResponseWriter, r *http.Request) {
	sessionName := r.FormValue("newSessionName")
	if redirectToCorrectSession(sessionName, w, r) {
		return
	}
	newCoderName := r.FormValue("newCoderName")
	// "\n" used as separator when returning list of users later.
	newCoderName = strings.Replace(newCoderName, "\n", " ", -1)
	fmt.Println("JOIN SESSION " + sessionName)
	session := sessionMap[sessionName]
	if session == nil {
		sessionMap[sessionName] = createSession()
		go receiveMulticast(sessionName)
	}
	session = sessionMap[sessionName]
	if _, ok := session.userMap[newCoderName]; ok {
		// User already exists here.
		fmt.Fprintf(w, "FAILURE")
	} else {
		// TODO Multicast list of active users.
		session.userMap[newCoderName] = &userInfo{}
		// TODO Should there be a timeout / removal mechanism for inactive users?
		fmt.Fprintf(w, "SUCCESS")
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

// TODO Maybe update master? We're changing the session map.
func receiveMulticast(sessionName string) {
	ch := caster.GetMessageChan(sessionName)
	for {
		mi := <-ch
		if masterId == -1 {
			fmt.Printf("Multicast received: Setting master to %d\n", mi.MasterId)
			masterId = mi.MasterId
		}
		sessionName := mi.SessionName
		session := sessionMap[sessionName]
		if session == nil {
			sessionMap[sessionName] = createSession()
			session = sessionMap[sessionName]
		}
		codeToExecute := mi.CodeToExecute
		if codeToExecute != "" {
			sendExecuteRequestToSessionMaster(session, codeToExecute)
		}
	}
}

func usage() {
	fmt.Printf("usage: %s serverName\n", os.Args[0])
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
	
	// initialize mapElection by Servers
	for i, _ := range configuration.Servers {
		if i == len(configuration.Servers) - 1 {
			mapElection[i] = 0
		} else {
			mapElection[i] = i + 1
		}
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

	// initialize multicast
	caster.Initialize(configuration.Servers[serverId].Port)
	for i, server := range configuration.Servers {
		if serverId == i {
			continue
		}
		if configuration.Groups[groupId].Name == server.Group {
			caster.AddMember(server.Name, server.IP+":"+server.Port)
		}
	}
	// debug only
	showConfiguration()

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
	}

	http.ListenAndServe(":"+configuration.Servers[serverId].HttpPort, nil)
}
