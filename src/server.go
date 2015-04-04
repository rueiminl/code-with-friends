package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"multicaster"
	"hash/fnv"
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
		Group string    `json:"group"`
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
	validPath     = regexp.MustCompile("^/(readsessionactive|readexecutedcode|executecode|edit|resetsession|joinsession)/([a-zA-Z0-9]*)$")
	sessionMap    = make(map[string]*PythonSession)
	configuration = new(Configuration)
	serverId      = -1
	groupId       = -1
	caster        = new(multicaster.Multicaster)
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
		fn(w, r)
	}
}
func homeHandler(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "edit")
}
func editHandler(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "edit")
}
func executecodeHandler(w http.ResponseWriter, r *http.Request) {
	// Parse necessary code to be excuted...
	codeToExecute := r.FormValue("codeToExecute")
	fmt.Fprintf(w, "Hey, you want me to execute this: "+codeToExecute)
	codeToExecute += "\n" // Required to terminate command.
	if strings.Count(codeToExecute, "\n") > 0 {
		codeToExecute += "\n" // Double termination (possibly) required for multiline commands.
	}

	// TODO WHEN MULTIPLE SESSIONS EXIST Lookup the sesion here.
	sessionName := r.FormValue("sessionName")
	session := sessionMap[sessionName]
	if session == nil {
		return
	}

	// ... and send that code to the master to be written to the session.
	_, ok := <-session.chMasterReady
	if ok {
		fmt.Println("writing to active session.")
		session.chExecuteCode <- codeToExecute // Must request that master handle session.
	} else {
		fmt.Println("No session active.")
	}
	// test only
	caster.Multicast(codeToExecute, 5)
}
func readexecutedcodeHandler(w http.ResponseWriter, r *http.Request) {
	urlPrefixLen := len("/readexecutedcode/")
	requestedIoNumber, err := strconv.Atoi(r.URL.Path[urlPrefixLen:])
	if err != nil {
		fmt.Println(err)
		return
	}

	// TODO WHEN MULTIPLE SESSIONS EXIST Lookup the sesion here.
	sessionName := r.FormValue("sessionName")
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
	// TODO WHEN MULTIPLE SESSIONS EXIST Lookup the sesion here.
	sessionName := r.FormValue("sessionName")
	session := sessionMap[sessionName]
	if session != nil && session.cmd != nil {
		fmt.Fprintf(w, "ACTIVE")
	} else {
		fmt.Fprintf(w, "DEAD")
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
	// argv := []string{"-i"}
	sessionName := r.FormValue("newSessionName")
	fmt.Println("JOIN SESSION " + sessionName)	
	session := sessionMap[sessionName]
	if session == nil {
		sessionMap[sessionName] = createSession()
	}
	
	// Todo redirect to the master server of group
	groupId := getGroupId(sessionName)
	fmt.Printf("the session should be handled by group %d\n", groupId);
}

func waitForSessionDeath(s *PythonSession) {
	s.cmd.Wait()
	// TODO Possibly handle closing channels? We don't want any stuck goroutines.
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

func receiveMulticast() {
	ch := caster.GetMessageChan()
	for {
		fmt.Println(<-ch)
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
		caster.AddMember(server.Name, server.IP + ":" + server.Port)
	}
	// debug only
	go receiveMulticast()
	showConfiguration()

	flag.Parse()
	http.HandleFunc("/", makeHandler(homeHandler))
	http.HandleFunc("/edit/", makeHandler(editHandler))
	http.HandleFunc("/executecode/", makeHandler(executecodeHandler))
	http.HandleFunc("/readexecutedcode/", makeHandler(readexecutedcodeHandler))
	http.HandleFunc("/readsessionactive/", makeHandler(readsessionactiveHandler))
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

	http.ListenAndServe(":" + configuration.Servers[serverId].HttpPort, nil)
}
