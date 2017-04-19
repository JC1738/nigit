package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/codegangsta/cli"
	"github.com/op/go-logging"
)

var log = logging.MustGetLogger("nigit")
var colorFormat = logging.MustStringFormatter(
	`%{color}%{level:.7s} ▶ %{message}%{color:reset}`,
)
var uncoloredFormat = logging.MustStringFormatter(
	`%{level:.7s} ▶ %{message}`,
)

// Execute program in a given time frame and deal and log errors
// if programm exits successfully the full stdout output is returned
func execProgram(program string, extraEnv []string, input string, timeout int) (bytes.Buffer, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	env := append(os.Environ(), extraEnv...)

	programName := filepath.Base(program)
	cmd := exec.Command(program)
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = env

	reportFailure := func() {
		log.Errorf(
			"Execution of program %s failed with %s\n%s\n%s",
			programName,
			cmd.ProcessState.String(),
			strings.Join(extraEnv, " "),
			strings.Trim(stderr.String(), "\n"))
	}

	if err := cmd.Start(); err != nil {
		reportFailure()
		return stdout, err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-time.After(time.Duration(timeout) * time.Second):
		if err := cmd.Process.Kill(); err != nil {
			log.Fatal("Cannot kill process: ", err)
			return stdout, err
		}
		log.Debugf("Process %s killed", programName)
	case err := <-done:
		if err != nil {
			reportFailure()
			return stdout, err
		}
		log.Debugf("Executed %s without error in %s", programName, cmd.ProcessState.UserTime())
	}

	return stdout, nil
}

// Get url route of a program path
func urlPath(programPath string) string {
	return "/" + strings.TrimSuffix(filepath.Base(programPath), filepath.Ext(programPath))
}

type JsonProgramArgs struct {
	Envs  []string
	Stdin string
}

func handleJson(r *http.Request) (envs []string, input *bytes.Buffer, err error) {
	decoder := json.NewDecoder(r.Body)

	var args JsonProgramArgs
	err = decoder.Decode(&args)
	if err != nil {
		return
	}

	envs = args.Envs
	input = bytes.NewBufferString(args.Stdin)
	return
}

func handleForm(r *http.Request) (envs []string, input *bytes.Buffer, err error) {
	input = new(bytes.Buffer)
	r.ParseMultipartForm(5 * 1000 * 1000)

	// If it is not a multipart upload the form can actuall be nil. Stupid design decision
	if r.MultipartForm != nil {
		if fileHeaders, ok := r.MultipartForm.File["stdin"]; ok {
			// If users uploaded a specific file with the name stdin we us this as source
			file, err := fileHeaders[0].Open()
			defer file.Close()

			if err != nil {
				return nil, nil, err
			}

			input.ReadFrom(file)
		}
	} else if stdinField, ok := r.Form["stdin"]; ok {
		// If users have a stdin field we pass that as input to the program
		input.WriteString(stdinField[0])
		delete(r.Form, "stdin")
	}

	// All form arguments are injected into the environment of the executed child program
	for k, v := range r.Form {
		envs = append(envs, fmt.Sprintf("%s=%s", strings.ToUpper(k), strings.Join(v, " ")))
	}

	return
}

func handleInput(w http.ResponseWriter, r *http.Request, programPath string, timeout int, envs []string, input *bytes.Buffer) {
	// Important HTTP headers are passed to the child program
	accept := r.Header.Get("Accept")
	envs = append(envs, "ACCEPT="+accept)
	envs = append(envs, "HOST="+r.Header.Get("Host"))

	stdout, _ := execProgram(programPath, envs, input.String(), timeout)

	// We reply with the requested content type as we do not know
	// what the program or script will ever return while the client does
	mediatype, _, err := mime.ParseMediaType(accept)

	//Force response language to En
	w.Header().Set("Content-Language", "en-US")
	if err == nil && mediatype != "*/*" {
		w.Header().Set("Content-Type", mediatype)
		stdout.WriteTo(w)
	} else {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, stdout.String())
	}

}

func serve(programPath string, timeout int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")

		switch contentType {
		case "application/json":
			envs, input, err := handleJson(r)
			if err != nil {
				log.Warningf("Invalid request %s", err)
				http.Error(w, err.Error(), http.StatusNotImplemented)
				return
			}
			handleInput(w, r, programPath, timeout, envs, input)
		default:
			envs, input, err := handleForm(r)
			if err != nil {
				log.Warningf("Invalid request %s", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			handleInput(w, r, programPath, timeout, envs, input)
		}

	})
}

func checkAuth(programPath string, timeout int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authToken := r.Header.Get("Authorization")
		envs := []string{"AUTH=" + authToken}

		stdout, err := execProgram(programPath, envs, "", timeout)
		if err != nil {
			stdout.WriteTo(os.Stdout)
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "{\"Error\":\"Unauthorized\"}")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func checkPath(program string) (string, error) {
	programPath, err := filepath.Abs(program)
	if err != nil {
		return "", fmt.Errorf("Cannot get path of %s", program)
	}

	programPath, err = exec.LookPath(programPath)
	if err != nil {
		return "", fmt.Errorf("Executable program %s not found", program)
	}

	return programPath, nil
}

func logRequests(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Infof("%s %s", r.Method, r.URL)
		handler.ServeHTTP(w, r)
	})
}

func main() {
	app := cli.NewApp()
	app.Name = "nigit"
	app.Version = "0.3"
	app.Usage = "Expose any Program as HTTP API"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "port",
			Value:  "8000",
			Usage:  "HTTP port",
			EnvVar: "PORT",
		},
		cli.IntFlag{
			Name:   "timeout",
			Value:  5,
			Usage:  "Timeout in seconds after process is stopped",
			EnvVar: "TIMEOUT",
		},
		cli.BoolFlag{
			Name:   "no-color",
			Usage:  "Do not colorize output",
			EnvVar: "NO_COLOR",
		},
		cli.StringFlag{
			Name:   "auth",
			Usage:  "Executable to call with authorization header",
			EnvVar: "AUTH",
		},
		cli.StringFlag{
			Name:   "cert",
			Usage:  "Certificate for https",
			EnvVar: "CERT",
		},
		cli.StringFlag{
			Name:   "key",
			Usage:  "Private key for https",
			EnvVar: "KEY",
		},
	}

	app.Action = func(c *cli.Context) {
		if !c.Args().Present() {
			fmt.Println("Please provide the names of the scripts to run under nigit")
			os.Exit(1)
		}

		if c.GlobalBool("no-color") {
			logging.SetFormatter(uncoloredFormat)
		} else {
			logging.SetFormatter(colorFormat)
		}

		log.Infof("Serve from port %s with %d seconds timeout", c.GlobalString("port"), c.GlobalInt("timeout"))

		for _, program := range c.Args() {
			programPath, err := checkPath(program)
			if err != nil {
				log.Fatal(err)
			}

			log.Infof("Handle %s -> %s", urlPath(programPath), program)
			if c.GlobalString("auth") != "" {
				authPath, err := checkPath(c.GlobalString("auth"))
				if err != nil {
					log.Fatal(err)
				}

				http.Handle(urlPath(programPath),
					checkAuth(authPath,
						c.GlobalInt("timeout"),
						serve(programPath, c.GlobalInt("timeout")),
					),
				)
			} else {
				http.Handle(urlPath(programPath), serve(programPath, c.GlobalInt("timeout")))
			}
		}

		if c.GlobalString("cert") != "" || c.GlobalString("key") != "" {
			http.ListenAndServeTLS(":"+c.GlobalString("port"),
				c.GlobalString("cert"),
				c.GlobalString("key"),
				logRequests(http.DefaultServeMux),
			)
		} else {
			http.ListenAndServe(":"+c.GlobalString("port"), logRequests(http.DefaultServeMux))
		}
	}

	app.Run(os.Args)
}
