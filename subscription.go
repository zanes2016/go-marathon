/*
Copyright 2014 Rohith All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package marathon

import (
	"encoding/json"
	"fmt"
	"github.com/donovanhide/eventsource"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// Subscriptions is a collection to urls that marathon is implementing a callback on
type Subscriptions struct {
	CallbackURLs []string `json:"callbackUrls"`
}

// Subscriptions retrieves a list of registered subscriptions
func (r *marathonClient) Subscriptions() (*Subscriptions, error) {
	subscriptions := new(Subscriptions)
	if err := r.apiGet(marathonAPISubscription, nil, subscriptions); err != nil {
		return nil, err
	}

	return subscriptions, nil
}

// AddEventsListener adds your self as a listener to events from Marathon
//		channel:	a EventsChannel used to receive event on
func (r *marathonClient) AddEventsListener(channel EventsChannel, filter int) error {
	r.Lock()
	defer r.Unlock()

	// step: someone has asked to start listening to event, we need to register for events
	// if we haven't done so already
	if err := r.registerSubscription(); err != nil {
		return err
	}

	if _, found := r.listeners[channel]; !found {
		r.listeners[channel] = filter
	}
	return nil
}

// RemoveEventsListener removes the channel from the events listeners
//		channel:			the channel you are removing
func (r *marathonClient) RemoveEventsListener(channel EventsChannel) {
	r.Lock()
	defer r.Unlock()

	if _, found := r.listeners[channel]; found {
		delete(r.listeners, channel)
		// step: if there is no one else listening, let's remove ourselves
		// from the events callback
		if r.config.EventsTransport == EventsTransportCallback && len(r.listeners) == 0 {
			r.Unsubscribe(r.SubscriptionURL())
		}
	}
}

// SubscriptionURL retrieves the subscription callback URL used when registering
func (r *marathonClient) SubscriptionURL() string {
	if r.config.CallbackURL != "" {
		return fmt.Sprintf("%s%s", r.config.CallbackURL, defaultEventsURL)
	}

	return fmt.Sprintf("http://%s:%d%s", r.ipAddress, r.config.EventsPort, defaultEventsURL)
}

// RegisterSubscription registers ourselves with Marathon to receive events from configured transport facility
func (r *marathonClient) registerSubscription() error {
	switch r.config.EventsTransport {
	case EventsTransportCallback:
		return r.registerCallbackSubscription()
	case EventsTransportSSE:
		return r.registerSSESubscription()
	default:
		return fmt.Errorf("the events transport: %d is not supported", r.config.EventsTransport)
	}
}

func (r *marathonClient) registerCallbackSubscription() error {
	if r.eventsHTTP == nil {
		ipAddress, err := getInterfaceAddress(r.config.EventsInterface)
		if err != nil {
			return fmt.Errorf("Unable to get the ip address from the interface: %s, error: %s",
				r.config.EventsInterface, err)
		}

		// step: set the ip address
		r.ipAddress = ipAddress
		binding := fmt.Sprintf("%s:%d", ipAddress, r.config.EventsPort)
		// step: register the handler
		http.HandleFunc(defaultEventsURL, r.handleCallbackEvent)
		// step: create the http server
		r.eventsHTTP = &http.Server{
			Addr:           binding,
			Handler:        nil,
			ReadTimeout:    10 * time.Second,
			WriteTimeout:   10 * time.Second,
			MaxHeaderBytes: 1 << 20,
		}

		// @todo need to add a timeout value here
		listener, err := net.Listen("tcp", binding)
		if err != nil {
			return nil
		}

		go func() {
			for {
				r.eventsHTTP.Serve(listener)
			}
		}()
	}

	// step: get the callback url
	callback := r.SubscriptionURL()

	// step: check if the callback is registered
	found, err := r.HasSubscription(callback)
	if err != nil {
		return err
	}
	if !found {
		// step: we need to register ourselves
		uri := fmt.Sprintf("%s?callbackUrl=%s", marathonAPISubscription, callback)
		if err := r.apiPost(uri, "", nil); err != nil {
			return err
		}
	}

	return nil
}

func (r *marathonClient) registerSSESubscription() error {
	// Prevent multiple SSE subscriptions
	if r.subscribedToSSE {
		return nil
	}

	var stream *eventsource.Stream

	// Try to connect to Marathon until succeed or
	// the whole custer is down
	for {
		// Get a member from the cluster
		marathon, err := r.cluster.GetMember()
		if err != nil {
			return err
		}
		url := fmt.Sprintf("%s/%s", marathon, marathonAPIEventStream)

		// Try to connect to stream
		stream, err = eventsource.Subscribe(url, "")
		if err == nil {
			break
		}

		log.Printf("failed to connect to Marathon event stream, error: %s", err)
		r.cluster.MarkDown()
	}

	go func() {
		for {
			select {
			case ev := <-stream.Events:
				r.handleEvent(ev.Data())
			case err := <-stream.Errors:
				log.Printf("failed to receive event, error: %s", err)
			}
		}
	}()

	r.subscribedToSSE = true
	return nil
}

// Unsubscribe removes ourselves from Marathon's callback facility
//	url		: the url you wish to unsubscribe
func (r *marathonClient) Unsubscribe(callbackURL string) error {
	// step: remove from the list of subscriptions
	return r.apiDelete(fmt.Sprintf("%s?callbackUrl=%s", marathonAPISubscription, callbackURL), nil, nil)
}

// HasSubscription checks to see a subscription already exists with Marathon
//		callback:			the url of the callback
func (r *marathonClient) HasSubscription(callback string) (bool, error) {
	// step: generate our events callback
	subscriptions, err := r.Subscriptions()
	if err != nil {
		return false, err
	}

	for _, subscription := range subscriptions.CallbackURLs {
		if callback == subscription {
			return true, nil
		}
	}

	return false, nil
}

func (r *marathonClient) handleEvent(content string) {
	// step: process and decode the event
	eventType := new(EventType)
	err := json.NewDecoder(strings.NewReader(content)).Decode(eventType)
	if err != nil {
		log.Printf("failed to decode the event type, content: %s, error: %s", content, err)
		return
	}

	// step: check whether event type is handled
	event, err := GetEvent(eventType.EventType)
	if err != nil {
		log.Printf("unable to handle event, type: %s, error: %s", eventType.EventType, err)
		return
	}

	// step: let's decode message
	err = json.NewDecoder(strings.NewReader(content)).Decode(event.Event)
	if err != nil {
		log.Printf("failed to decode the event, id: %d, error: %s", event.ID, err)
		return
	}

	r.RLock()
	defer r.RUnlock()

	// step: check if anyone is listen for this event
	for channel, filter := range r.listeners {
		// step: check if this listener wants this event type
		if event.ID&filter != 0 {
			go func(ch EventsChannel, e *Event) {
				ch <- e
			}(channel, event)
		}
	}
}

func (r *marathonClient) handleCallbackEvent(writer http.ResponseWriter, request *http.Request) {
	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		log.Printf("failed to read request body, error: %s", err)
		return
	}

	r.handleEvent(string(body[:]))
}
