// IoT Wifi Management

package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bhoriuchi/go-bunyan/bunyan"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/txn2/txwifi/iotwifi"
)

// ApiReturn structures a message for returned API calls.
type ApiReturn struct {
	Status  string      `json:"status"`
	Message string      `json:"message"`
	Payload interface{} `json:"payload"`
}

func main() {
	// Declare config file name
	const configFileName = "cfg/.app_config"

	logConfig := bunyan.Config{
		Name:   "txwifi",
		Stream: os.Stdout,
		Level:  bunyan.LogLevelDebug,
	}

	blog, err := bunyan.CreateLogger(logConfig)
	if err != nil {
		panic(err)
	}

	blog.Info("Starting IoT Wifi...")

	messages := make(chan iotwifi.CmdMessage, 1)

	cfgUrl := setEnvIfEmpty("IOTWIFI_CFG", "cfg/wificfg.json")
	port := setEnvIfEmpty("IOTWIFI_PORT", "8080")

	reconfig := readConfig(configFileName, "reconfig")

	go iotwifi.RunWifi(blog, messages, cfgUrl, reconfig)
	wpacfg := iotwifi.NewWpaCfg(blog, cfgUrl)

	// If reconfig state 1 then turn on webserver
	if reconfig == 1 {
		blog.Info("Turning on webserver")

		// Make new message channel to config message
		configMsg := make(chan bool)
		go func() {
			chk := <-configMsg
			blog.Info("Configuration done! Turning off AP and webserver")
			if chk == true {
				time.Sleep(5 * time.Second)
				wpacfg.DisableAP()
				time.Sleep(2 * time.Second)
				close(configMsg) // Closing channel
			}
		}()

		apiPayloadReturn := func(w http.ResponseWriter, message string, payload interface{}) {
			apiReturn := &ApiReturn{
				Status:  "OK",
				Message: message,
				Payload: payload,
			}
			ret, _ := json.Marshal(apiReturn)

			w.Header().Set("Content-Type", "application/json")
			w.Write(ret)
		}

		// marshallPost populates a struct with json in post body
		marshallPost := func(w http.ResponseWriter, r *http.Request, v interface{}) {
			bytes, err := ioutil.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				blog.Error(err)
				return
			}

			defer r.Body.Close()

			decoder := json.NewDecoder(strings.NewReader(string(bytes)))

			err = decoder.Decode(&v)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				blog.Error(err)
				return
			}
		}

		// common error return from api
		retError := func(w http.ResponseWriter, err error) {
			apiReturn := &ApiReturn{
				Status:  "FAIL",
				Message: err.Error(),
			}
			ret, _ := json.Marshal(apiReturn)

			w.Header().Set("Content-Type", "application/json")
			w.Write(ret)
		}

		// handle /status POSTs json in the form of iotwifi.WpaConnect
		statusHandler := func(w http.ResponseWriter, r *http.Request) {

			status, err := wpacfg.Status()
			if err != nil {
				blog.Error(err.Error())
				return
			}

			apiPayloadReturn(w, "status", status)
		}

		// handle /connect POSTs json in the form of iotwifi.WpaConnect
		connectHandler := func(w http.ResponseWriter, r *http.Request) {
			var creds iotwifi.WpaCredentials
			marshallPost(w, r, &creds)

			blog.Info("Connect Handler Got: ssid:|%s| psk:|%s|", creds.Ssid, creds.Psk)

			connection, err := wpacfg.ConnectNetwork(creds)
			if err != nil {
				blog.Error(err.Error())
				return
			}

			apiReturn := &ApiReturn{
				Status:  "OK",
				Message: "Connection",
				Payload: connection,
			}

			ret, err := json.Marshal(apiReturn)
			if err != nil {
				retError(w, err)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.Write(ret)

			/*
			* Reset config file after reconfiguration
			* conf = based on configFileName
			* val = is the value we have to change followed by `conf` order
			 */
			conf := []string{"reconfiguration"}
			val := []string{"0"}
			editConfig(configFileName, conf, val)

			// Send to channel to turnoff AP
			configMsg <- true
		}

		// scan for wifi networks
		scanHandler := func(w http.ResponseWriter, r *http.Request) {
			blog.Info("Got Scan")
			wpaNetworks, err := wpacfg.ScanNetworks()
			if err != nil {
				retError(w, err)
				return
			}

			apiReturn := &ApiReturn{
				Status:  "OK",
				Message: "Networks",
				Payload: wpaNetworks,
			}

			ret, err := json.Marshal(apiReturn)
			if err != nil {
				retError(w, err)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.Write(ret)
		}

		// kill the application
		killHandler := func(w http.ResponseWriter, r *http.Request) {
			messages <- iotwifi.CmdMessage{Id: "kill"}

			apiReturn := &ApiReturn{
				Status:  "OK",
				Message: "Killing service.",
			}
			ret, err := json.Marshal(apiReturn)
			if err != nil {
				retError(w, err)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.Write(ret)
		}

		// common log middleware for api
		logHandler := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				staticFields := make(map[string]interface{})
				staticFields["remote"] = r.RemoteAddr
				staticFields["method"] = r.Method
				staticFields["url"] = r.RequestURI

				blog.Info(staticFields, "HTTP")
				next.ServeHTTP(w, r)
			})
		}

		// setup router and middleware
		r := mux.NewRouter()
		r.Use(logHandler)

		// set app routes
		r.HandleFunc("/status", statusHandler)
		r.HandleFunc("/connect", connectHandler).Methods("POST")
		r.HandleFunc("/scan", scanHandler)
		r.HandleFunc("/kill", killHandler)
		http.Handle("/", r)

		// CORS
		headersOk := handlers.AllowedHeaders([]string{"Content-Type", "Authorization", "Content-Length", "X-Requested-With", "Accept", "Origin"})
		originsOk := handlers.AllowedOrigins([]string{"*"})
		methodsOk := handlers.AllowedMethods([]string{"GET", "HEAD", "POST", "PUT", "OPTIONS", "DELETE"})

		// serve http
		blog.Info("HTTP Listening on " + port)
		http.ListenAndServe(":"+port, handlers.CORS(originsOk, headersOk, methodsOk)(r))

		// Just in case
		foreverLoop()
	} else {
		blog.Info("Webserver no need to turned on")
		foreverLoop()
	}
}

func foreverLoop() {
	for {
		select {}
	} // Block
}

// getEnv gets an environment variable or sets a default if
// one does not exist.
func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if len(value) == 0 {
		return fallback
	}

	return value
}

// setEnvIfEmp<ty sets an environment variable to itself or
// fallback if empty.
func setEnvIfEmpty(env string, fallback string) (envVal string) {
	envVal = getEnv(env, fallback)
	os.Setenv(env, envVal)

	return envVal
}

func readConfig(fileName string, configName string) int {
	// Read all data from file name
	input, err := ioutil.ReadFile(fileName)
	if err != nil {
		log.Fatalln(err)
	}

	// Split every line
	lines := strings.Split(string(input), "\n")

	var val int

	// Loop all the lines, and change configName val
	for _, line := range lines {
		if strings.Contains(line, configName) {
			tmp := strings.Split(line, "=")
			val, err = strconv.Atoi(tmp[1])
			if err != nil {
				log.Fatalln(err)
			}
		}
	}

	return val
}

func editConfig(fileName string, configName []string, val []string) {

	// Read all data from file name
	input, err := ioutil.ReadFile(fileName)
	if err != nil {
		log.Fatalln(err)
	}

	// Split every line
	lines := strings.Split(string(input), "\n")

	// Loop all the lines, and change configName val
	for i, line := range lines {
		for x := 0; x < len(configName); x++ {
			if strings.Contains(line, configName[x]) {
				lines[i] = configName[x] + "=" + val[x]
			}
		}
	}

	// Join all the splited text
	output := strings.Join(lines, "\n")

	// Write all over again
	err = ioutil.WriteFile(fileName, []byte(output), 0644)
	if err != nil {
		log.Fatalln(err)
	}
}
