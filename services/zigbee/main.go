// Service to translate zigbee2mqtt messages to/from gohome.
package zigbee

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/barnybug/gohome/pubsub/mqtt"

	"github.com/barnybug/gohome/pubsub"
	"github.com/barnybug/gohome/services"
	MQTT "github.com/eclipse/paho.mqtt.golang"
)

// Service zigbee
type Service struct {
}

func (self *Service) ID() string {
	return "zigbee"
}

var topicMap = map[string]string{
	"state":       "ack",
	"temperature": "temp",
}
var fieldMap = map[string]string{
	"temperature": "temp",
}

func getDevice(topic string) string {
	ps := strings.Split(topic, "/")
	return ps[len(ps)-1]
}

var deviceUpdate = regexp.MustCompile(`^zigbee2mqtt/[^/]+$`)

var dedup = map[string]string{}

func checkDup(message MQTT.Message) bool {
	payload := string(message.Payload())
	if last, ok := dedup[message.Topic()]; ok && payload == last {
		return true
	}
	dedup[message.Topic()] = payload
	return false
}

type LogMessage struct {
	Message string `json:"message"`
	Meta    struct {
		Description  string `json:"description"`
		FriendlyName string `json:"friendly_name"`
	} `json:"meta"`
}

func checkLogMessage(message MQTT.Message) {
	// announce
	var msg LogMessage
	err := json.Unmarshal(message.Payload(), &msg)
	if err != nil {
		log.Printf("Failed to parse message %s: '%s'", message.Topic(), message.Payload())
	}
	if msg.Message != "interview_successful" {
		return
	}
	// announce new devices
	source := fmt.Sprintf("zigbee.%s", msg.Meta.FriendlyName)
	log.Printf("Announcing %s: %s", source, msg.Meta.Description)
	fields := pubsub.Fields{"source": source, "name": msg.Meta.Description}
	ev := pubsub.NewEvent("announce", fields)
	services.Config.AddDeviceToEvent(ev)
	services.Publisher.Emit(ev)
}

func translate(message MQTT.Message) *pubsub.Event {
	if message.Topic() == "zigbee2mqtt/bridge/log" {
		checkLogMessage(message)
		return nil
	}
	if strings.HasPrefix(message.Topic(), "zigbee2mqtt/bridge/") {
		// ignore other bridge messages
		return nil
	}
	if strings.HasSuffix(message.Topic(), "/set") {
		// ignore reflected set
		return nil
	}
	if !deviceUpdate.MatchString(message.Topic()) {
		log.Printf("Ignoring topic: %s", message.Topic())
		return nil
	}
	if checkDup(message) {
		return nil
	}

	var data map[string]interface{}
	err := json.Unmarshal(message.Payload(), &data)
	if err != nil {
		log.Printf("Failed to parse message %s: '%s'", message.Topic(), message.Payload())
		return nil
	}

	device := getDevice(message.Topic())
	source := fmt.Sprintf("zigbee.%v", device)
	topic := "zigbee"
	fields := pubsub.Fields{
		"source": source,
	}
	for key, value := range data {
		if topicValue, ok := topicMap[key]; ok {
			// use presence of keys to determine topic
			topic = topicValue
		}
		// map fields
		if key == "state" {
			fields["command"] = strings.ToLower(value.(string))
		} else if key == "brightness" {
			fields["level"] = DimToPercentage(int(value.(float64)))
		} else if to, ok := fieldMap[key]; ok {
			fields[to] = value
		} else {
			fields[key] = value // map unknowns as is
		}
	}
	ev := pubsub.NewEvent(topic, fields)
	services.Config.AddDeviceToEvent(ev)
	return ev
}

func (self *Service) handleCommand(ev *pubsub.Event) {
	id, ok := services.Config.LookupDeviceProtocol(ev.Device(), "zigbee")
	if !ok {
		return // command not for us
	}
	device := services.Config.Devices[ev.Device()]
	command := ev.Command()
	if command != "off" && command != "on" {
		log.Println("Command not recognised:", command)
		return
	}
	log.Printf("Setting device %s to %s\n", ev.Device(), command)

	// translate to zigbee2mqtt message
	topic := fmt.Sprintf("zigbee2mqtt/%s/set", id)
	body := map[string]interface{}{}
	body["state"] = strings.ToUpper(command)
	if ev.IsSet("level") {
		body["brightness"] = PercentageToDim(int(ev.IntField("level")))
	}
	temp := ev.IntField("temp")
	if temp > 0 {
		if device.Cap["colourtemp"] {
			mirek := int(1_000_000 / temp)
			body["color_temp"] = mirek
		} else {
			// emulate colour temperature with x/y/dim
			x, y, dim := KelvinToColorXYDim(int(temp))
			body["color"] = map[string]interface{}{"x": x, "y": y}
			if !ev.IsSet("level") {
				body["brightness"] = dim
			}
		}
	}
	if ev.IsSet("colour") {
		body["color"] = map[string]interface{}{"hex": ev.StringField("colour")}
	}
	payload, _ := json.Marshal(body)
	log.Println("Sending", topic, string(payload))
	token := mqtt.Client.Publish(topic, 1, false, payload)
	if token.Wait() && token.Error() != nil {
		log.Println("Failed to publish message:", token.Error())
	}
}

func (self *Service) Run() error {
	mqtt.Client.Subscribe("zigbee2mqtt/#", 1, func(client MQTT.Client, msg MQTT.Message) {
		ev := translate(msg)
		if ev != nil {
			services.Publisher.Emit(ev)
		}
	})

	commandChannel := services.Subscriber.FilteredChannel("command")
	for {
		select {
		case command := <-commandChannel:
			self.handleCommand(command)
		}
	}
	return nil
}
