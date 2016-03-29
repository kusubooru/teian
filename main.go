package main

import (
	"crypto/md5"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/context"

	"github.com/kusubooru/teian/shimmie"
	"github.com/kusubooru/teian/store"
	"github.com/kusubooru/teian/store/datastore"
	"github.com/kusubooru/teian/teian"
)

var (
	httpAddr = flag.String("http", "localhost:8080", "HTTP listen address")
	dbDriver = flag.String("driver", "mysql", "Database driver")
	dbConfig = flag.String("config", "", "username:password@(host:port)/database?parseTime=true")
	boltFile = flag.String("boltfile", "teian.db", "BoltDB database file to store suggestions")
	loginURL = flag.String("loginurl", "/suggest/login", "Login URL path to redirect to")
	certFile = flag.String("tlscert", "", "TLS public key in PEM format.  Must be used together with -tlskey")
	keyFile  = flag.String("tlskey", "", "TLS private key in PEM format.  Must be used together with -tlscert")
	// Set after flag parsing based on certFile & keyFile.
	useTLS bool
)

const description = `Usage: teian [options]
  A service that allows users to submit suggestions.
Options:
`

func usage() {
	fmt.Fprintf(os.Stderr, description)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\n")
}

func main() {
	flag.Usage = usage
	flag.Parse()
	useTLS = *certFile != "" && *keyFile != ""

	// create database connection and store
	s := datastore.Open(*dbDriver, *dbConfig, *boltFile)
	closeStoreOnSignal(s)
	// add store to context
	ctx := store.NewContext(context.Background(), s)

	http.Handle("/suggest", shimmie.Auth(ctx, serveIndex, *loginURL))
	http.Handle("/suggest/admin", shimmie.Auth(ctx, serveAdmin, *loginURL))
	http.Handle("/suggest/admin/delete", shimmie.Auth(ctx, handleDelete, *loginURL))
	http.Handle("/suggest/submit", shimmie.Auth(ctx, handleSubmit, *loginURL))
	http.Handle("/suggest/login", http.HandlerFunc(serveLogin))
	http.Handle("/suggest/login/submit", newHandler(ctx, handleLogin))
	http.Handle("/suggest/logout", http.HandlerFunc(handleLogout))

	if useTLS {
		if err := http.ListenAndServeTLS(*httpAddr, *certFile, *keyFile, nil); err != nil {
			log.Fatalf("Could not start listening (TLS) on %v: %v", *httpAddr, err)
		}
	} else {
		if err := http.ListenAndServe(*httpAddr, nil); err != nil {
			log.Fatalf("Could not start listening on %v: %v", *httpAddr, err)
		}
	}
}

func closeStoreOnSignal(s store.Store) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)
	go func() {
		for sig := range c {
			log.Printf("%v signal received, releasing database resources and exiting...", sig)
			s.Close()
			os.Exit(1)
		}
	}()
}

type ctxHandlerFunc func(context.Context, http.ResponseWriter, *http.Request)

func newHandler(ctx context.Context, fn ctxHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn(ctx, w, r)
	}
}

func serveIndex(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	render(w, suggestionTmpl, nil)
}

func serveLogin(w http.ResponseWriter, r *http.Request) {
	render(w, loginTmpl, nil)
}

func serveAdmin(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	user, ok := ctx.Value("user").(*teian.User)
	if !ok {
		http.Redirect(w, r, *loginURL, http.StatusFound)
		return
	}
	if user.Admin != "Y" {
		http.Error(w, "You are not authorized to view this page.", http.StatusUnauthorized)
		return
	}
	suggs, err := store.GetAllSugg(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error: %v", err), http.StatusInternalServerError)
	}

	u := r.FormValue("u")
	t := r.FormValue("t")
	o := r.FormValue("o")
	// filter
	if u != "" {
		suggs = teian.FilterByUser(suggs, u)
	}
	if t != "" {
		suggs = teian.FilterByText(suggs, t)
	}

	// order
	switch o {
	case "ua":
		sort.Sort(teian.ByUser(suggs))
	case "ud":
		sort.Sort(sort.Reverse(teian.ByUser(suggs)))
	case "da":
		sort.Sort(teian.ByDate(suggs))
	case "dd":
		fallthrough
	default:
		sort.Sort(sort.Reverse(teian.ByDate(suggs)))
	}
	render(w, listTmpl, suggs)
}

func handleDelete(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// only accept POST method
	if r.Method != "POST" {
		http.Error(w, fmt.Sprintf("%v method not allowed", r.Method), http.StatusMethodNotAllowed)
		return
	}
	user, ok := ctx.Value("user").(*teian.User)
	if !ok {
		http.Redirect(w, r, *loginURL, http.StatusFound)
		return
	}
	if user.Admin != "Y" {
		http.Error(w, "You are not authorized to perform this action.", http.StatusUnauthorized)
		return
	}
	idValue := r.PostFormValue("id")
	username := r.PostFormValue("username")
	if idValue == "" || username == "" {
		http.Error(w, "id and username must be present", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseUint(idValue, 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad id provided: %v", err), http.StatusBadRequest)
		return
	}
	err = store.DeleteSugg(ctx, username, id)
	if err != nil {
		http.Error(w, fmt.Sprintf("delete suggestion failed: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/suggest/admin", http.StatusFound)
}

func handleLogin(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// only accept POST method
	if r.Method != "POST" {
		http.Error(w, fmt.Sprintf("%v method not allowed", r.Method), http.StatusMethodNotAllowed)
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	user, err := store.GetUser(ctx, username)
	if err != nil {
		log.Println(err)
		render(w, loginTmpl, "User does not exist")
		return
	}
	hash := md5.Sum([]byte(username + password))
	passwordHash := fmt.Sprintf("%x", hash)
	if user.Pass != passwordHash {
		render(w, loginTmpl, "Username and password do not match")
		return
	}
	addr := strings.Split(r.RemoteAddr, ":")[0]
	cookieValue := shimmie.CookieValue(passwordHash, addr)
	shimmie.SetCookie(w, "shm_user", username)
	shimmie.SetCookie(w, "shm_session", cookieValue)
	http.Redirect(w, r, "/suggest", http.StatusFound)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	shimmie.SetCookie(w, "shm_user", "")
	shimmie.SetCookie(w, "shm_session", "")
	http.Redirect(w, r, "/suggest", http.StatusFound)
}

func handleSubmit(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// only accept POST method
	if r.Method != "POST" {
		http.Error(w, fmt.Sprintf("%v method not allowed", r.Method), http.StatusMethodNotAllowed)
		return
	}
	// get user from context
	user, ok := ctx.Value("user").(*teian.User)
	if !ok {
		http.Redirect(w, r, *loginURL, http.StatusFound)
		return
	}
	text := r.PostFormValue("text")
	// redirect if suggestion text is empty
	if len(strings.TrimSpace(text)) == 0 {
		http.Redirect(w, r, "/suggest", http.StatusFound)
		return
	}

	type result struct {
		Err  error
		Msg  string
		Type string
	}

	// create and store suggestion
	err := store.CreateSugg(ctx, user.Name, &teian.Sugg{Text: text})
	if err != nil {
		render(w, submitTmpl, result{Err: err, Type: "error", Msg: "Something broke! :'( Our developers were notified."})
	}

	render(w, submitTmpl, result{Type: "success", Msg: "Your suggestion has been submitted. Thank you for your feedback!"})

}

func render(w http.ResponseWriter, t *template.Template, data interface{}) {
	if err := t.Execute(w, data); err != nil {
		log.Print(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

var (
	suggestionTmpl = template.Must(template.New("").Parse(baseTemplate + subnavTemplate + suggestionTemplate))
	submitTmpl     = template.Must(template.New("").Parse(baseTemplate + subnavTemplate + submitTemplate))
	listTmpl       = template.Must(template.New("").Parse(baseTemplate + subnavTemplate + toolbarTemplate + listTemplate))
	loginTmpl      = template.Must(template.New("").Parse(baseTemplate + loginTemplate))
)

const (
	baseTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<title>suggest</title>
	<style>
		* {
			font-size: 16px; 
			line-height: 1.2;
			font-family: Verdana, Geneva, sans-serif;
		}
		a:link {
			color:#006FFA;
			text-decoration:none;
		}
		a:visited {
			color:#006FFA;
			text-decoration:none;
		}
		a:hover {
			color:#33CFFF;
			text-decoration:none;
		}
		a:active {
			color:#006FFA;
			text-decoration:none;
		}

		p, a, span {font-size: 120%;}

		body {
			list-style-type: none;
			padding-top: 0;
			margin-top: 0;
		}

		#site-title {
			font-size: 133%;
			padding: 0.5em;
			margin: 0;
		}

		#subnav {
		    background: #f6f6f6;
			padding-top: 1em;
			padding-bottom: 1em;
			border-top: 1px #ebebeb solid;
			border-bottom: 1px #ebebeb solid;
		}

		#subnav a {
		    padding: 0.5em;
		}

		#subnav subnav-button-link {
		    padding: 0.5em;
		}

		.subnav-button-form {
			display: inline;
		}

		.subnav-button-link {
			background: none!important;
			border: none;
			padding: 0!important;
			font: inherit;
			font-size: 120%;
			cursor: pointer;
			color: #006FFA;
			display: inline;
		}

		.subnav-button-link:visited {
			color:#006FFA;
			text-decoration:none;
		}
		.subnav-button-link:hover {
			color:#33CFFF;
			text-decoration:none;
		}
		.subnav-button-link:active {
			color:#006FFA;
			text-decoration:none;
		}

		#login-form label, #login-form input, #login-form button, #login-form em {
			padding: 0.5em;
			display: block;
			font-size: 120%;
			line-height:1.2;
		}

		#login-form button {
			margin-top: 0.5em;
		}

		#login-form h1 {
			padding: 0.5em;
			font-size: 120%;
		}

		.toolbar {
			padding: 0.5em;
		}
		.toolbar input, .toolbar button {
			font-size: 120%;
		}

		.suggestion {
			padding: 0.5em;
			border-top: 1px #ebebeb solid;
			border-bottom: 1px #ebebeb solid;
			border-left: 0.3em #006FFA solid;
			border-top-left-radius: 0.3em;
			border-bottom-left-radius: 0.3em;
			line-height: 200%;
		}

		.suggestion form {
			display: inline;
		}

		.suggestion textarea {
			display: block;
			font-size: 120%;
			line-height:1.2;
		}
		.suggestion:nth-of-type(even) {
		    background: #f6f6f6;
		}

		.suggestion-form {
			padding: 0.5em;
			line-height: 200%;
		}

		textarea {
			width: 70%;
		}
		@media (max-width: 768px) {
			textarea {
				width: 100%;
			}
		}

		.suggestion-form input[type=submit] {
			padding: 0.5em;
			margin-top: 0.5em;
			display: block;
		}

		.alert {
			border-radius: 4px;
			padding: 1em;
			margin-top: 0.5em;
			margin-bottom: 0.5em;
			font-size: 120%;
			width: 70%;
		}
		.alert strong {
			font-size: inherit;
		}
		@media (max-width: 768px) {
			.alert{
				width: 90%;
				padding-left:5%;
				padding-right:5%;
			}
		}

		.alert-success {
			color: #3c763d;
			background-color: #dff0d8;
			border-color: #d6e9c6;
		}

		.alert-error {
			color: #a94442;
			background-color: #f2dede;
			border-color: #ebccd1;
		}



	</style>
</head>
<body>
	<h1 id="site-title"><a href="/post/list">Kusubooru</a></h1>
	{{block "subnav" .}}{{end}}
	{{block "toolbar" .}}{{end}}
	{{block "content" .}}{{end}}
</body>
</html>
`
	toolbarTemplate = `
{{define "toolbar"}}
<div class="toolbar">
	<form method="get" action="/suggest/admin">
		<input type="text" name="u" placeholder="Username">
		<input type="text" name="t" placeholder="Text">
		<label for="order">Order By</label>
		<select id="order" name="o">
			<option value="dd">Date Desc</option>
			<option value="da">Date Asc</option>
			<option value="ud">Username Desc</option>
			<option value="ua">Username Asc</option>
		</select>
	    <button type="submit">Search</button>
		<input type="reset" value="Reset">
	</form>
</div>
{{end}}
`
	subnavTemplate = `
{{define "subnav"}}
<div id="subnav">
	<a href="/suggest">New suggestion</a>
	<form class="subnav-button-form" method="post" action="/suggest/logout">
	     <input class="subnav-button-link" type="submit" value="Logout">
	</form>
</div>
{{end}}
`
	suggestionTemplate = `
{{define "content"}}
<div class="suggestion-form">
	<form method="post" action="/suggest/submit">
		<p>Write your suggestion</p>
		<textarea class="large" rows="20" cols="80" name="text" placeholder="Write your suggestion here."></textarea>
		<input type="submit">
	</form>
</div>
{{end}}
`
	submitTemplate = `
{{define "content"}}
{{ if .Err }}
<!-- Error -->
<!-- .Err  -->
<!----------->
{{end}}
{{ if .Msg }}
<div class="alert alert-{{.Type}}">
	<strong>
	{{if eq .Type "success"}}
	Success:
	{{else}}
	Error:
	{{end}}
	</strong>{{.Msg}}
</div>
{{end}}
{{end}}
`
	listTemplate = `
{{define "content"}}
{{ range $k, $v := . }}
	<div class="suggestion">
		<span>{{$v.FmtCreated}} by <a href="/user/{{$v.Username}}">{{$v.Username}}</a></span>
		<form method="post" action="/suggest/admin/delete">
			<input type="hidden" name="username" value="{{$v.Username}}">
			<input type="hidden" name="id" value="{{$v.ID}}">
			<input type="submit" value="Delete">
		</form>
		<textarea cols="80" readonly>{{$v.Text}}</textarea>
	</div>
{{ end }}
{{end}}
`

	loginTemplate = `
{{define "content"}}
<form id="login-form" method="post" action="/suggest/login/submit">
<h1>Login</h1>
    <label for="username">User name</label>
    <input type="text" id="username" name="username">
    <label for="password">Password</label>
    <input type="password" id="password" name="password">
    <button type="submit">Login</button>
	{{if .}}
	<em>{{.}}</em>
	{{end}}
</form>
{{end}}
`
)
