package main

import (
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"syscall"
)

type Page struct {
	Title string
	Body  []byte
}

var (
	addr        = flag.Bool("addr", false, "find open address and print to final-port.txt")
	gopath      = os.Getenv("GOPATH")
	webpagesDir = gopath + "webpages/"
	validPath   = regexp.MustCompile("^/(code|edit|save|newsession)/([a-zA-Z0-9]*)$")
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
		fmt.Println("Call from: " + r.URL.Path)
		m := validPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			fmt.Println("Not found.")
			http.NotFound(w, r)
			return
		} else {
			fmt.Println(m)
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
func codeHandler(w http.ResponseWriter, r *http.Request) {
	codeToExecute := r.FormValue("codeToExecute")
	fmt.Println("Code to execute: " + codeToExecute)
	fmt.Fprintf(w, "Hey, you want me to execute this: "+codeToExecute)
}
func newsessionHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("NEW SESSION")
	argv := []string{"-i"}
	binary, err := exec.LookPath("python")
	pid, err := syscall.ForkExec(binary, argv, nil)
	if err != nil {
		fmt.Println("error: ")
		fmt.Println(err)
	} else {
		fmt.Println("Created a new process: ")
		// TODO: Currently able to create a new process. However, you
		// need to be able to pipe input/output, have 'kill' command ready.
		fmt.Println(pid)
	}
}

func main() {
	flag.Parse()
	http.HandleFunc("/edit/", makeHandler(editHandler))
	http.HandleFunc("/save/", makeHandler(saveHandler))
	http.HandleFunc("/code/", makeHandler(codeHandler))
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
