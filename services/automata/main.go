// Service for state machine based automation of behaviour. A whole variety of
// complex behaviour can be achieved by linking together triggering events and
// actions.
//
// This is the powerful glue behind the whole gohome system that links the dumb
// input/output services together in smart ways.
//
// Some examples:
//
// - debouncing the door bell
//
// - alert via twitter, sms, etc. when mail arrives
//
// - switch lights on when people get home
//
// - unlocking an electric door lock when an rfid tag is presented
//
// - when the sunsets turn on lights
//
// - a presence based smart burglar alarm system (when the house is empty, turn on the burglar alarm)
//
// The automata are configured via yaml configuration format configured under:
//
// http://localhost:8723/config?path=gohome/config/automata
//
// An example of the configuration is available in the gohome github repository.
//
// For more details on the configuration format, see:
//
// http://godoc.org/github.com/barnybug/gofsm
package automata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/Knetic/govaluate"
	"github.com/barnybug/gohome/util"

	"github.com/barnybug/gofsm"

	"github.com/barnybug/gohome/config"
	"github.com/barnybug/gohome/pubsub"
	"github.com/barnybug/gohome/services"
)

var eventsLogPath = config.LogPath("events.log")

func openLogFile() *os.File {
	logfile, err := os.OpenFile(eventsLogPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		log.Println("Couldn't open events.log:", err)
		logfile, _ = os.Open(os.DevNull)
		return logfile
	}
	return logfile
}

// Service automata
type Service struct {
	timers            map[string]*time.Timer
	configUpdated     chan bool
	log               *os.File
	functions         map[string]govaluate.ExpressionFunction
	restoredAutomaton map[string]bool
	rand              *rand.Rand
}

var automata *gofsm.Automata

// ID of the service
func (self *Service) ID() string {
	return "automata"
}

var reAction = regexp.MustCompile(`(\w+)\((.+)\)`)

type EventContext struct {
	service *Service
	event   *pubsub.Event
}

func (c EventContext) Get(name string) (interface{}, error) {
	switch name {
	case "type":
		return strings.SplitN(c.event.Device(), ".", 2)[0], nil
	case "topic":
		return c.event.Topic, nil
	case "timestamp":
		return c.event.Timestamp, nil
	default:
		return c.event.Fields[name], nil
	}
}

func NewEventContext(service *Service, event *pubsub.Event) EventContext {
	return EventContext{service, event}
}

func State(args ...interface{}) (interface{}, error) {
	if len(args) != 1 {
		return nil, errors.New("Expected 1 arguments to State()")
	}
	name := args[0].(string)
	aut, ok := automata.Automaton[name]
	if !ok {
		return nil, fmt.Errorf("State(): automata '%s' not found", name)
	}

	return aut.State.Name, nil
}

var parsingCache = map[string]*govaluate.EvaluableExpression{}

func (self *Service) defineFunctions() {
	// govaluate functions
	self.functions = map[string]govaluate.ExpressionFunction{
		"State":       State,
		"Alert":       self.Alert,
		"Command":     self.Command,
		"Log":         self.Log,
		"Query":       self.Query,
		"Script":      self.Script,
		"Snapshot":    self.Snapshot,
		"StartTimer":  self.StartTimer,
		"RandomTimer": self.RandomTimer,
	}
}

func (self *Service) ParseCached(s string) (*govaluate.EvaluableExpression, error) {
	if expr, ok := parsingCache[s]; ok {
		return expr, nil
	}

	if self.functions == nil {
		self.defineFunctions()
	}

	expr, err := govaluate.NewEvaluableExpressionWithFunctions(s, self.functions)
	if err != nil {
		return nil, fmt.Errorf("Bad expression '%s': %s", s, err)
	}
	parsingCache[s] = expr
	return expr, nil
}

func (self EventContext) Match(when string) bool {
	expr, err := self.service.ParseCached(when)
	if err != nil {
		log.Printf("Error parsing expression '%s': %s", when, err)
		return false
	}
	result, err := expr.Eval(self)
	if err != nil {
		log.Printf("Error evaluating expression '%s': %s", when, err)
		return false
	}
	result, ok := result.(bool)
	if !ok {
		log.Printf("Expression didn't evaluate to boolean '%s'", when)
	}
	return result.(bool)
}

func (self EventContext) String() string {
	s := self.event.Device()
	for k, v := range self.event.Fields {
		if k == "device" {
			continue
		}
		s += fmt.Sprintf(" %s=%v", k, v)
	}
	return s
}

func (self *Service) ConfigUpdated(path string) {
	// trigger reload in main loop
	self.configUpdated <- true
}

func (self *Service) RestoreFile(automata *gofsm.Automata) {
	r, err := os.Open(config.ConfigPath("automata.state"))
	if err != nil {
		log.Println("Restoring automata state failed:", err)
		return
	}
	dec := json.NewDecoder(r)
	var p gofsm.AutomataState
	err = dec.Decode(&p)
	if err != nil {
		log.Println("Restoring automata state failed:", err)
		return
	}
	automata.Restore(p)
}

func (self *Service) QueryHandlers() services.QueryHandlers {
	return services.QueryHandlers{
		"status": services.TextHandler(self.queryStatus),
		"switch": services.TextHandler(self.querySwitch),
		"logs":   services.TextHandler(self.queryLogs),
		"script": services.TextHandler(self.queryScript),
		"state":  services.TextHandler(self.queryState),
		"help": services.StaticHandler("" +
			"status: get status\n" +
			"switch device on|off: switch device\n" +
			"logs: get recent event logs\n" +
			"script: run a script\n" +
			"state: manually update automaton state"),
	}
}

func (self *Service) queryStatus(q services.Question) string {
	var out string
	now := time.Now()
	var keys []string
	for k := range automata.Automaton {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	group := ""
	for _, k := range keys {
		if k == "events" {
			continue
		}
		if strings.Split(k, ".")[0] != group {
			group = strings.Split(k, ".")[0]
			out += group + "\n"
		}
		device := k
		if dev, ok := services.Config.Devices[k]; ok {
			device = dev.Name
		}
		aut := automata.Automaton[k]
		du := util.ShortDuration(now.Sub(aut.Since))
		out += fmt.Sprintf("- %s: %s for %s\n", device, aut.State.Name, du)
	}
	return out
}

type DummyEvent struct{}

func (d DummyEvent) String() string {
	return "user"
}

func (d DummyEvent) Match(s string) bool {
	return false
}

func (self *Service) queryState(q services.Question) string {
	args := strings.Split(q.Args, " ")
	if len(args) != 2 {
		return "usage: state automata state"
	}
	aut, ok := automata.Automaton[args[0]]
	if !ok {
		return fmt.Sprintf("automata: '%s' not found", args[0])
	}
	_, ok = aut.States[args[1]]
	if !ok {
		return fmt.Sprintf("automata state: '%s' not found", args[1])
	}
	aut.ChangeState(args[1], DummyEvent{})
	return fmt.Sprintf("Change %s state to %s", args[0], args[1])
}

func keywordArgs(args []string) map[string]string {
	ret := map[string]string{}
	for _, arg := range args {
		p := strings.SplitN(arg, "=", 2)
		if len(p) == 2 {
			ret[p[0]] = p[1]
		} else {
			ret[""] = p[0]
		}
	}
	return ret
}

func isSwitchable(dev config.DeviceConf) bool {
	return dev.Cap["switch"]
}

func matchDevices(n string) []string {
	if _, ok := services.Config.Devices[n]; ok {
		return []string{n}
	}

	matches := []string{}
	for name, dev := range services.Config.Devices {
		if strings.Contains(name, n) && isSwitchable(dev) {
			matches = append(matches, name)
		}
	}
	return matches
}

func parseInt(str string, def int) int {
	if num, err := strconv.Atoi(str); err == nil {
		return num
	}
	return def
}

func (self *Service) querySwitch(q services.Question) string {
	if q.Args == "" {
		// return a list of the devices
		devices := []string{}
		for dev, _ := range services.Config.Devices {
			devices = append(devices, dev)
		}
		sort.Strings(devices)
		return strings.Join(devices, ", ")
	}
	args := strings.Split(q.Args, " ")
	name := args[0]
	matches := matchDevices(name)
	if len(matches) == 0 {
		return fmt.Sprintf("device %s not found", name)
	}
	if len(matches) > 1 {
		return fmt.Sprintf("device %s is ambiguous", strings.Join(matches, ", "))
	}

	dev := services.Config.Devices[matches[0]]
	// rest of key=value arguments
	command, fields := parseArgs(args[1:])
	sendCommand(matches[0], command, fields)
	return fmt.Sprintf("Switched %s %s", dev.Name, command)
}

func parseArgs(args []string) (string, pubsub.Fields) {
	kwargs := keywordArgs(args)
	command := "on"
	fields := pubsub.Fields{}
	for field, value := range kwargs {
		if field == "" {
			command = value
		} else if num, err := strconv.ParseFloat(value, 64); err == nil {
			fields[field] = num
		} else {
			fields[field] = value
		}
	}
	return command, fields
}

func sendCommand(name string, command string, params pubsub.Fields) {
	ev := pubsub.NewEvent("command", params)
	ev.SetField("command", command)
	ev.SetField("device", name)
	services.Publisher.Emit(ev)
}

func (self *Service) queryScript(q services.Question) string {
	output, err := script(q.Args)
	if err != nil {
		return fmt.Sprintf("Script failed: %s", err)
	}
	return output
}

func tail(filename string, lines int64) ([]byte, error) {
	n := fmt.Sprintf("-n%d", lines)
	return exec.Command("tail", n, filename).Output()
}

func (self *Service) queryLogs(q services.Question) string {
	data, err := tail(eventsLogPath, 25)
	if err != nil {
		return fmt.Sprintf("Couldn't retrieve logs: %s", err)
	}
	return string(data)
}

type MultiError []error

func (m MultiError) Error() string {
	s := ""
	for i, err := range m {
		if i > 0 {
			s += ", "
		}
		s += err.Error()
	}
	return s
}

func (self *Service) loadAutomata() error {
	c := services.Configured["config/automata"]
	tmpl := template.New("Automata")
	tmpl, err := tmpl.Parse(c.Get())
	if err != nil {
		return err
	}
	context := map[string]interface{}{
		"devices": services.Config.Devices,
	}

	wr := new(bytes.Buffer)
	err = tmpl.Execute(wr, context)
	if err != nil {
		return err
	}
	generated := wr.Bytes()
	updated, err := gofsm.Load(generated)
	if err != nil {
		return err
	}

	// precompile expressions
	errors := MultiError{}
	parsingCache = map[string]*govaluate.EvaluableExpression{}
	for _, aut := range updated.Automaton {
		for _, transition := range aut.Transitions {
			for _, t := range transition {
				_, err := self.ParseCached(t.When)
				if err != nil {
					errors = append(errors, err)
				}

				for _, action := range t.Actions {
					_, err := self.ParseCached(action)
					if err != nil {
						errors = append(errors, err)
					}
				}
			}
		}

		for _, state := range aut.States {
			for _, action := range state.Entering {
				_, err := self.ParseCached(action)
				if err != nil {
					errors = append(errors, err)
				}
			}
			for _, action := range state.Leaving {
				_, err := self.ParseCached(action)
				if err != nil {
					errors = append(errors, err)
				}
			}
		}
	}

	if len(errors) > 0 {
		return errors
	}

	automata = updated
	return nil
}

func timeit(name string, fn func()) {
	t1 := time.Now()
	fn()
	t2 := time.Now()
	log.Printf("%s took: %s", name, t2.Sub(t1))
}

func (self *Service) restoreState(ev *pubsub.Event) {
	k := ev.Device()
	if aut, ok := automata.Automaton[k]; ok {
		state := gofsm.AutomataState{}
		state[k] = gofsm.AutomatonState{ev.StringField("state"), ev.Timestamp}
		automata.Restore(state)
		log.Printf("Restored %s: %s at %s", k, aut.State.Name, aut.Since.Format(time.RFC3339))
		self.restoredAutomaton[k] = true
	}
}

func handleCommand(ev *pubsub.Event) {
	if strings.HasPrefix(ev.Device(), "scene.") {
		// simply ack the scene. This allows automata to handle running scripts,
		// and perform state changes as necessary to the ack event.
		fields := pubsub.Fields{
			"device":  ev.Device(),
			"command": ev.Command(),
		}
		ev := pubsub.NewEvent("ack", fields)
		services.Publisher.Emit(ev)
	}
}

func (self *Service) performAction(action gofsm.Action) {
	code := action.Name
	// rewrite expressions to add implicit 'context' first parameter
	// Might do away with this.
	code = strings.Replace(code, "(", "(context, ", 1)
	expr, err := self.ParseCached(code)
	if err != nil {
		log.Println("Error parsing action:", err)
		return
	}

	e := action.Trigger.(EventContext)
	params := map[string]interface{}{
		"context": ChangeContext{e.event, action.Change},
	}
	// log.Printf("Running: '%s'", code)
	_, err = expr.Evaluate(params)
	if err != nil {
		log.Printf("Error action '%s': %s", action.Name, err)
	}
}

func (self *Service) stateRestored() {
	for k, aut := range automata.Automaton {
		if _, ok := self.restoredAutomaton[k]; !ok {
			// automaton defaulted to initial state - 'persist' it
			publishState(k, aut.State.Name, "initial")
			log.Printf("Initial state %s: %s", k, aut.State.Name)
			self.restoredAutomaton[k] = true
		}
	}
}

func changeState(change gofsm.Change) {
	s := fmt.Sprintf("%-17s %s->%s", "["+change.Automaton+"]", change.Old, change.New)
	log.Printf("%-40s (event: %s)", s, change.Trigger)
	// emit event
	publishState(change.Automaton, change.New, fmt.Sprint(change.Trigger))
}

func publishState(device, state, trigger string) {
	fields := pubsub.Fields{
		"device":  device,
		"state":   state,
		"trigger": trigger,
	}
	ev := pubsub.NewEvent("state", fields)
	ev.SetRetained(true)
	services.Publisher.Emit(ev)
}

// Run the service
func (self *Service) Run() error {
	self.log = openLogFile()
	self.timers = map[string]*time.Timer{}
	self.configUpdated = make(chan bool, 2)
	self.restoredAutomaton = map[string]bool{}
	self.rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	// load templated automata
	err := self.loadAutomata()
	if err != nil {
		return err
	}

	// setup channels
	ch := services.Subscriber.Channel()
	defer services.Subscriber.Close(ch)
	stateRestored := time.NewTimer(time.Second * 5)
	earth := earthChannel()
	clock := util.NewScheduler(time.Duration(0), time.Minute)

	for {
		select {
		case ev := <-ch:
			if ev.Topic == "command" {
				handleCommand(ev)
				// ignore direct commands - ack/homeeasy events indicate commands completing.
				continue
			}
			if ev.Retained {
				if ev.Topic == "state" {
					self.restoreState(ev)
				}
				// ignore retained events from reconnecting
				continue
			}

			// send relevant events to the automata
			event := NewEventContext(self, ev)
			automata.Process(event)

		case change := <-automata.Changes:
			changeState(change)

		case action := <-automata.Actions:
			self.performAction(action)

		case <-self.configUpdated:
			// live reload the automata!
			log.Println("Automata config updated, reloading")
			state := automata.Persist()
			err := self.loadAutomata()
			if err != nil {
				log.Println("Failed to reload automata:", err)
				continue
			}
			automata.Restore(state)
			log.Println("Automata reloaded successfully")
			self.stateRestored()

		case <-stateRestored.C:
			// all retained State has been restored. Persist any missing State initial states.
			self.stateRestored()

		case tev := <-earth:
			ev := pubsub.NewEvent("earth",
				pubsub.Fields{"device": "earth", "command": tev.Event})
			services.Publisher.Emit(ev)
		case tick := <-clock.C:
			ev := pubsub.NewEvent("clock",
				pubsub.Fields{"device": "clock", "time": tick.Format("1504")})
			services.Publisher.Emit(ev)
		}
	}
	return nil
}

func (self *Service) appendLog(msg string) {
	now := time.Now()
	logMsg := fmt.Sprintf("%s: %s", now.Format(time.StampMilli), msg)
	fmt.Fprintln(self.log, logMsg)

	fields := pubsub.Fields{
		"message": msg,
		"source":  "event",
	}
	ev := pubsub.NewEvent("log", fields)
	services.Publisher.Emit(ev)
}

type ChangeContext struct {
	event  *pubsub.Event
	change gofsm.Change
}

func (c ChangeContext) Get(name string) (interface{}, bool) {
	device := c.event.Device()
	d := services.Config.Devices[device]
	now := time.Now()
	switch name {
	case "id":
		return d.Id, true
	case "name":
		return d.Name, true
	case "type":
		return strings.Split(d.Id, ".")[0], true
	case "cap":
		return d.Caps[0], true
	case "group":
		return d.Group, true
	case "duration":
		return util.FriendlyDuration(c.change.Duration), true
	case "timestamp":
		return now.Format(time.Kitchen), true
	case "datetime":
		return now.Format(time.StampMilli), true
	default:
		if value, ok := c.event.Fields[name]; ok {
			return value, true
		} else {
			return nil, false
		}
	}
}

var reSub = regexp.MustCompile(`\$(\w+)`)

func (c ChangeContext) Format(msg string) string {
	return reSub.ReplaceAllStringFunc(msg, func(k string) string {
		if value, ok := c.Get(k[1:]); ok {
			return fmt.Sprint(value)
		} else {
			return k
		}
	})
}

func checkArguments(args []interface{}, types ...string) error {
	if len(args) != len(types) {
		return fmt.Errorf("Expected %d arguments, but got %d", len(types), len(args))
	}
	for i, arg := range args {
		switch types[i] {
		case "string":
			if _, ok := arg.(string); !ok {
				return fmt.Errorf("Expected %s for argument %d, but got %v", types[i], i, arg)
			}
		case "float64":
			if n, ok := arg.(int); ok {
				args[i] = float64(n) // convert int->float64
			} else if n, ok := arg.(int64); ok {
				args[i] = float64(n) // convert int64->float64
			} else if _, ok := arg.(float64); !ok {
				return fmt.Errorf("Expected %s for argument %d, but got %v", types[i], i, arg)
			}
		}
	}

	return nil
}

func (self *Service) Log(args ...interface{}) (interface{}, error) {
	if err := checkArguments(args, "", "string"); err != nil {
		return nil, err
	}
	context := args[0].(ChangeContext)
	msg := args[1].(string)
	msg = context.Format(msg)
	self.appendLog(msg)
	log.Println("Log: ", msg)
	return nil, nil
}

func (self *Service) Script(args ...interface{}) (interface{}, error) {
	if err := checkArguments(args, "", "string"); err != nil {
		return nil, err
	}
	context := args[0].(ChangeContext)
	cmd := args[1].(string)
	cmd = context.Format(cmd)
	asyncScript(cmd)
	return nil, nil
}

func script(command string) (string, error) {
	args := strings.Split(command, " ")
	name := path.Base(args[0])
	if name == "" {
		return "", errors.New("Expected a script name argument")
	}
	args = args[1:]
	cmd := path.Join(util.ExpandUser(services.Config.General.Scripts), name)
	log.Println("Running script:", cmd)
	output, err := exec.Command(cmd, args...).CombinedOutput()
	return string(output), err
}

func asyncScript(command string) {
	// run non-blocking
	go func() {
		output, err := script(command)
		if err != nil {
			log.Printf("Script '%s' failed: %s", command, err)
		}
		output = strings.TrimSpace(output)
		log.Printf("Script '%s' successful: %s", command, output)
	}()
}

func (self *Service) Alert(args ...interface{}) (interface{}, error) {
	if err := checkArguments(args, "", "string", "string"); err != nil {
		return nil, err
	}
	context := args[0].(ChangeContext)
	msg := args[1].(string)
	target := args[2].(string)
	msg = context.Format(msg)
	log.Printf("%s: %s", strings.Title(target), msg)
	services.SendAlert(msg, target, "", 0)
	return nil, nil
}

func (self *Service) Query(args ...interface{}) (interface{}, error) {
	if err := checkArguments(args, "", "string"); err != nil {
		return nil, err
	}
	query := args[1].(string)
	log.Printf("Query %s", query)
	services.QueryChannel(query, time.Second*5)
	// results currently discarded
	return nil, nil
}

func (self *Service) Command(args ...interface{}) (interface{}, error) {
	if err := checkArguments(args, "", "string"); err != nil {
		return nil, err
	}
	context := args[0].(ChangeContext)
	text := args[1].(string)
	text = context.Format(text)
	// log.Printf("Sending %s", text)
	argv := strings.Split(text, " ")
	device := argv[0]
	command, fields := parseArgs(argv[1:])
	sendCommand(device, command, fields)
	return nil, nil
}

func (self *Service) Snapshot(args ...interface{}) (interface{}, error) {
	if err := checkArguments(args, "", "string", "string", "string"); err != nil {
		return nil, err
	}
	context := args[0].(ChangeContext)
	device := args[1].(string)
	target := args[2].(string)
	msg := args[3].(string)
	msg = context.Format(msg)
	ev := pubsub.NewCommand(device, "snapshot")
	ev.SetField("message", msg)
	ev.SetField("notify", target)
	services.Publisher.Emit(ev)
	return nil, nil
}

func (self *Service) startTimer(name string, d float64) {
	log.Printf("Starting timer: %s for %.1fs", name, d)
	duration := time.Duration(d) * time.Second
	if timer, ok := self.timers[name]; ok {
		// cancel any existing
		timer.Stop()
	}

	timer := time.AfterFunc(duration, func() {
		// emit timer event
		fields := pubsub.Fields{
			"device":  "timer." + name,
			"command": "on",
		}
		ev := pubsub.NewEvent("timer", fields)

		services.Publisher.Emit(ev)
	})
	self.timers[name] = timer
}

func (self *Service) StartTimer(args ...interface{}) (interface{}, error) {
	if err := checkArguments(args, "", "string", "float64"); err != nil {
		return nil, err
	}
	name := args[1].(string)
	d := args[2].(float64)
	self.startTimer(name, d)
	return nil, nil
}

func (self *Service) RandomTimer(args ...interface{}) (interface{}, error) {
	if err := checkArguments(args, "", "string", "float64", "float64"); err != nil {
		return nil, err
	}
	name := args[1].(string)
	min := args[2].(float64)
	max := args[3].(float64)
	if max <= min {
		return nil, errors.New("RandomTimer max must be greater than min")
	}
	d := self.rand.Float64()*(max-min) + min
	self.startTimer(name, d)
	return nil, nil
}
