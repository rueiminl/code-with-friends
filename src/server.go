package main

import (
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
)

type Page struct {
	Title string
	Body  []byte
}

type PythonSession struct {
	inPipe   io.WriteCloser
	outPipe  io.ReadCloser
	errPipe  io.ReadCloser
	cmd      *exec.Cmd
	ioNumber int            // Current index into ioMap. Starts at zero.
	ioMap    map[int]string // Map of all input/output/errors
}

var (
	addr        = flag.Bool("addr", false, "find open address and print to final-port.txt")
	gopath      = os.Getenv("GOPATH")
	webpagesDir = gopath + "webpages/"
	validPath   = regexp.MustCompile("^/(readsessionactive|readexecutedcode|executecode|edit|save|newsession)/([a-zA-Z0-9]*)$")
	session     = new(PythonSession) // TODO Add ability to construct multiple distinct sessions.
)

func (p *Page) save() error {
	filename := p.Title + ".txt"
	return ioutil.WriteFile(webpagesDir+filename, p.Body, 0600)
}

func loadPage(title string) (*Page, error) {
	fmt.Println("Load page: " + title)
	filename := title + ".txt"
	body, err := ioutil.ReadFile(webpagesDir + filename)
	if err != nil {
		return nil, err
	}
	return &Page{Title: title, Body: body}, nil
}

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
func saveHandler(w http.ResponseWriter, r *http.Request) {
	body := r.FormValue("body")
	p := &Page{Title: "abc", Body: []byte(body)}
	err := p.save()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/edit/", http.StatusFound)
}
func executecodeHandler(w http.ResponseWriter, r *http.Request) {
	codeToExecute := r.FormValue("codeToExecute")
	fmt.Println("Code to execute: " + codeToExecute)
	fmt.Fprintf(w, "Hey, you want me to execute this: "+codeToExecute)
	if session.cmd != nil {
		fmt.Println("writing to active session.")
		codeToExecute += "\n\n"
		writeToSession(codeToExecute, session)                   // TODO make async
		session.ioMap[session.ioNumber] = "INP:" + codeToExecute // TODO move to session master.
		session.ioNumber++
	} else {
		fmt.Println("No session active.")
	}
}
func readexecutedcodeHandler(w http.ResponseWriter, r *http.Request) {
	// TODO This function will require a significant overhaul!
	urlPrefixLen := len("/readexecutedcode/")
	requestedIoNumber, err := strconv.Atoi(r.URL.Path[urlPrefixLen:])
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(requestedIoNumber)
	//ioNumber   int            // Current index into ioMap. Starts at zero.
	//ioMap      map[int]string // Map of all input/output/errors
	fmt.Println(session.ioNumber)
	if session.ioNumber > requestedIoNumber { // Client is catching up...
		fmt.Println("Result: " + session.ioMap[requestedIoNumber])
		fmt.Fprintf(w, session.ioMap[requestedIoNumber])
	} else if session.ioNumber < requestedIoNumber { // Client is ahead?
		fmt.Println("Client is ahead?")
		fmt.Fprintf(w, "ZERO")
	} else { // Client should wait.
		fmt.Fprintf(w, "")
	}
}
func readsessionactiveHandler(w http.ResponseWriter, r *http.Request) {
	if session.cmd != nil {
		fmt.Fprintf(w, "ACTIVE")
	} else {
		fmt.Fprintf(w, "DEAD")
	}
}

func newsessionHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("NEW SESSION")
	//argv := []string{"-i"}
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
	fmt.Println("Created a new process: ")
	// TODO: Currently able to create a new process. However, you
	// need to be able to pipe input/output, have 'kill' command ready.
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
	go handleSessionOutput(session)
	err = session.cmd.Start()
	if err != nil {
		fmt.Println("Start cannot run")
		fmt.Println(err)
		return
	}
	//fmt.Println("About to do print hello world command")
	//writeToSession("print 'hello world'\n", session)
	//fmt.Println("about to wait...")
	go waitForSessionDeath()
}

func waitForSessionDeath() {
	//err :=
	session.cmd.Wait()
	// TODO identify session as nil. Need ONE goroutine modifying session. Aka a "master" goroutine.
}

func handlePythonSingleOutput(outPipe io.ReadCloser, outChannel chan<- string) {
	output := make([]byte, 1000)
	for {
		n, err := outPipe.Read(output)
		if err != nil {
			fmt.Println("(out/err) ERROR")
			fmt.Println(err)
			close(outChannel)
			return
		} else {
			fmt.Println("(out/err) saw: " + string(output[:n]))
			outChannel <- string(output[:n])
		}
	}
}

func handleSessionOutput(s *PythonSession) {
	outPipe := s.outPipe
	errPipe := s.errPipe
	outChannel := make(chan string, 1)
	errChannel := make(chan string, 1)
	go handlePythonSingleOutput(outPipe, outChannel)
	go handlePythonSingleOutput(errPipe, errChannel)

	for {
		select {
		case out, ok := <-outChannel:
			if !ok {
				fmt.Println("Closing output channel")
				outChannel = nil
			} else {
				fmt.Println("(outChan): " + out)
				s.ioMap[s.ioNumber] = "OUT:" + out // TODO move to session master.
				s.ioNumber++
			}
		case err, ok := <-errChannel:
			if !ok {
				fmt.Println("Closing err channel")
				errChannel = nil
			} else {
				fmt.Println("(errChan): " + err)
				s.ioMap[s.ioNumber] = "ERR:" + err // TODO move to session master.
				s.ioNumber++
			}
		}

		if outChannel == nil && errChannel == nil {
			return
		}
	}
}

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

func main() {
	flag.Parse()
	http.HandleFunc("/", makeHandler(homeHandler))
	http.HandleFunc("/edit/", makeHandler(editHandler))
	http.HandleFunc("/save/", makeHandler(saveHandler))
	http.HandleFunc("/executecode/", makeHandler(executecodeHandler))
	http.HandleFunc("/readexecutedcode/", makeHandler(readexecutedcodeHandler))
	http.HandleFunc("/readsessionactive/", makeHandler(readsessionactiveHandler))
	http.HandleFunc("/newsession/", makeHandler(newsessionHandler))

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

	http.ListenAndServe(":8080", nil)
}
