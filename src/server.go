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
)

type Page struct {
	Title string
	Body  []byte
}

type PythonSession struct {
	inPipe     io.WriteCloser
	outPipe    io.ReadCloser
	errPipe    io.ReadCloser
	cmd        *exec.Cmd
	lastinput  string
	lastoutput string // TODO Figure out a better way to store this info
	lasterror  string
}

var (
	addr        = flag.Bool("addr", false, "find open address and print to final-port.txt")
	gopath      = os.Getenv("GOPATH")
	webpagesDir = gopath + "webpages/"
	validPath   = regexp.MustCompile("^/(readexecutedcode|executecode|edit|save|newsession)/([a-zA-Z0-9]*)$")
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

func renderTemplate(w http.ResponseWriter, tmpl string, p *Page) {
	fmt.Println("Rendering: " + webpagesDir + tmpl + ".html")
	t, err := template.ParseFiles(webpagesDir + tmpl + ".html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = t.Execute(w, p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func makeHandler(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := validPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			fmt.Println("Not found.")
			http.NotFound(w, r)
			return
		}
		fn(w, r)
	}
}
func editHandler(w http.ResponseWriter, r *http.Request) {
	p, err := loadPage("abc")
	if err != nil {
		fmt.Println("[edit] cannot load page.")
		p = &Page{Title: "abc"}
	}
	renderTemplate(w, "edit", p)
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
	codeToExecute += "\n"
	session.lastinput = codeToExecute
	if session.cmd != nil {
		fmt.Println("writing to active session.")
		writeToSession(codeToExecute, session)
	} else {
		fmt.Println("No session active.")
	}
}
func readexecutedcodeHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("HANDLER")
	// TODO This function will require a significant overhaul!
	if session.lastinput != "" {
		fmt.Fprintf(w, "IN: "+session.lastinput)
		session.lastinput = ""
		fmt.Println("INPUT")
	} else if session.lastoutput != "" {
		fmt.Fprintf(w, "OUT: "+session.lastoutput)
		session.lastoutput = ""
		fmt.Println("OUTPUT")
	} else if session.lasterror != "" {
		fmt.Fprintf(w, "ERR: "+session.lasterror+"\n")
		session.lasterror = ""
		fmt.Println("ERROR")
	} else {
		fmt.Fprintf(w, "")
		fmt.Println("NOTHING TO REPORT...")
	}
}

func newsessionHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("NEW SESSION")
	//argv := []string{"-i"}
	argv := "-i"
	binary, err := exec.LookPath("python")
	session.cmd = exec.Command(binary, argv)
	if session.cmd == nil {
		fmt.Println("error: ")
		fmt.Println(err)
		return
	}
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
	fmt.Println("About to do print hello world command")
	writeToSession("print 'hello world'\n", session)
	fmt.Println("about to wait...")
	go session.cmd.Wait()
	/*
		if err != nil {
			fmt.Println("Wait error.")
			fmt.Println(err)
		} else {
			fmt.Println("Waited successfully. Session dead.")
		}
	*/
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
			fmt.Println("(outChan): " + out)
			s.lastoutput = out
			if !ok {
				fmt.Println("Closing output channel")
				outChannel = nil
			}
		case err, ok := <-errChannel:
			fmt.Println("(errChan): " + err)
			s.lasterror = err
			if !ok {
				fmt.Println("Closing err channel")
				errChannel = nil
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
	http.HandleFunc("/edit/", makeHandler(editHandler))
	http.HandleFunc("/save/", makeHandler(saveHandler))
	http.HandleFunc("/executecode/", makeHandler(executecodeHandler))
	http.HandleFunc("/readexecutedcode/", makeHandler(readexecutedcodeHandler))
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
