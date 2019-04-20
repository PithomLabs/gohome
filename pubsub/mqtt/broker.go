package mqtt

import (
	"fmt"
	"log"
	"os"

	"github.com/barnybug/gohome/pubsub"
	MQTT "github.com/eclipse/paho.mqtt.golang"
)

type Broker struct {
	broker     string
	subscriber *Subscriber
	client     MQTT.Client
}

var Client MQTT.Client

func createClientOpts(broker, name string) *MQTT.ClientOptions {
	// generate a client id
	hostname, _ := os.Hostname()
	clientID := fmt.Sprintf("gohome/%s-%s", hostname, name)
	opts := MQTT.NewClientOptions()
	opts.AddBroker(broker)
	opts.SetClientID(clientID)
	// ensure subscriptions survive across disconnections
	opts.SetCleanSession(false)
	return opts
}

func NewBroker(broker, name string) *Broker {
	opts := createClientOpts(broker, name)
	ret := &Broker{broker: broker}
	ret.subscriber = NewSubscriber(ret)
	opts.SetDefaultPublishHandler(ret.subscriber.publishHandler)

	client := MQTT.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalln("Couldn't Start mqtt:", token.Error())
	}
	Client = client
	ret.client = client
	return ret
}

func (self *Broker) Id() string {
	return "mqtt: " + self.broker
}

func (self *Broker) Subscriber() pubsub.Subscriber {
	return self.subscriber
}

func (self *Broker) Publisher() *Publisher {
	ch := make(chan *pubsub.Event)
	return &Publisher{broker: self.broker, channel: ch, client: self.client}
}
