package main

import (
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"fmt"

	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/miekg/dns"
)

// MustReadFile returns the contents of a file or panics
func MustReadFile(path string) []byte {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatalln(err)
	}
	return data
}

// MustGetAddress returns the IPv4 address for an interface or panics
func MustGetAddress(interfaceName string) net.IP {
	iface, err := net.InterfaceByName("en0")

	if err == nil {
		if addrs, err := iface.Addrs(); err == nil {
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip.To4() != nil {
					return ip
				}
			}
		} else {
			panic(err)
		}
	}
	panic(err)
}

// MustRR returns a dns.RR from a template or panics
func MustRR(template string) dns.RR {
	value, err := dns.NewRR(template)
	if err != nil {
		panic(err)
	}
	return value
}

// Host contains the external interface address to bind to
var Host = MustGetAddress("en0").To4().String()

// WriteError puts a server failure message on the response
func WriteError(response dns.ResponseWriter, request *dns.Msg) {
	message := &dns.Msg{}
	message.SetReply(request)
	message.Compress = false
	message.RecursionAvailable = true
	message.SetRcode(request, dns.RcodeServerFailure)
	response.WriteMsg(message)
}

// RecurseHandler creates a handler that will query the next responding Nameserver
func (config *Config) RecurseHandler(response dns.ResponseWriter, request *dns.Msg) {
	questions := request.Question

	for _, nameserver := range config.Nameservers {
		c := &dns.Client{Net: string(nameserver.Transport), Timeout: time.Duration(nameserver.Timeout)}
		var r *dns.Msg
		var rtt time.Duration
		var err error

		r, rtt, err = c.Exchange(request, nameserver.Address)
		if err == nil || err == dns.ErrTruncated {
			r.Compress = false

			// Forward the response
			log.Printf("RecurseHandler: recurse RTT for %v (%v)", questions, rtt)
			if err := response.WriteMsg(r); err != nil {
				log.Printf("RecurseHandler: failed to respond: %v", err)
			}
			return
		}
		log.Printf("RecurseHandler: recurse failed: %v", err)
	}

	// If all resolvers fail, return a SERVFAIL message
	log.Printf("RecurseHandler: all resolvers failed for %v from client %s (%s)",
		questions, response.RemoteAddr().String(), response.RemoteAddr().Network())

	WriteError(response, request)
}

// RequestHandler returns a function that will look up entries in a Config
func (config *Config) RequestHandler(response dns.ResponseWriter, request *dns.Msg) {
	message := new(dns.Msg)

	var answers []dns.RR
	var unknown []dns.Question

	for _, question := range request.Question {
		key := strings.TrimSuffix(question.Name, fmt.Sprintf(".%s", config.Name))

		record := config.Records[key]
		if record != nil {
			answers = append(answers, MustRR(fmt.Sprintf("%s %d IN A %s", question.Name, record.TTL, record.Address)))
		} else {
			unknown = append(unknown, question)
		}
	}

	if len(unknown) > 0 {
		log.Printf("Failed to resolve: %q, recursing.", unknown)

		answers = append(answers, config.Resolve(unknown)...)
	}

	message.Answer = answers

	message.Ns = []dns.RR{
		MustRR(fmt.Sprintf("%s 3600 IN NS %s.", config.Name, Host)),
	}

	message.Authoritative = true
	message.RecursionAvailable = true
	message.SetReply(request)

	response.WriteMsg(message)
}

// Record contains a single DNS entry
type Record struct {
	Address string
	TTL     int
}

// NamedRecord contains a DNS entry and its name
type NamedRecord struct {
	Record
	Name string
}

// Transport is either the string "tcp" or "udp"
type Transport string

// UnmarshalJSON parses a transport string
func (e *Transport) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*e = "tcp"
	} else if s == "tcp" || s == "udp" {
		*e = Transport(s)
	} else {
		return fmt.Errorf("Illegal value for transport %q", s)
	}
	return nil
}

// Duration can be JSON parsed
type Duration time.Duration

// UnmarshalJSON parses a string into a time.Duration
func (e *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	duration, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*e = Duration(duration)
	return nil
}

// Nameserver will respond if we do not know an entry
type Nameserver struct {
	Address   string
	Timeout   Duration
	Transport Transport
}

// Client creates a DNS client to a nameserver
func (nameserver *Nameserver) Client() *dns.Client {
	return &dns.Client{
		Net:     string(nameserver.Transport),
		Timeout: time.Duration(nameserver.Timeout),
	}
}

// Config contains global server configuration
type Config struct {
	Port        int
	Name        string
	Secret      string
	Records     map[string]*Record
	Nameservers []Nameserver
}

// Resolve a list of questions
func (config *Config) Resolve(questions []dns.Question) []dns.RR {
	targets := make(map[string]dns.Question)
	var answers []dns.RR

	for _, question := range questions {
		targets[question.Name] = question
	}

	for _, nameserver := range config.Nameservers {
		if len(targets) == 0 {
			break
		}

		var unknown []dns.Question

		for _, question := range targets {
			unknown = append(unknown, question)
		}

		r := new(dns.Msg)
		r.Question = questions

		c := nameserver.Client()

		r, _, err := c.Exchange(r, nameserver.Address)
		if err == nil || err == dns.ErrTruncated {
			answers = append(answers, r.Answer...)

			for _, answer := range r.Answer {
				delete(targets, answer.Header().Name)
			}
		} else {
			log.Printf("DNS resolve failed: %v", err)
		}
	}
	return answers
}

// CheckSecret returns gin middleware to verify a shared secret header
func (config *Config) CheckSecret() gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.Join(c.Request.Header["Secret"], "") == config.Secret {
			c.Next()
		} else {
			c.String(403, "Incorrect shared secret")
			c.Abort()
		}
	}
}

func main() {
	var config Config

	if err := json.Unmarshal(MustReadFile("config.json"), &config); err != nil {
		log.Fatalln(err)
	}

	ip := fmt.Sprintf("%s:%d", Host, config.Port)

	log.Println(fmt.Sprintf("DNS on %s", ip))

	for _, protocol := range []string{"udp", "tcp"} {
		go func(server *dns.Server) {
			if err := server.ListenAndServe(); err != nil {
				log.Fatalln(err)
			}
			log.Fatalln("DNS server crashed")
		}(&dns.Server{Addr: ip, Net: protocol})
	}

	dns.HandleFunc(config.Name, config.RequestHandler)
	dns.HandleFunc(".", config.RecurseHandler)

	router := gin.Default()

	api := router.Group("/api")
	{
		api.Use(config.CheckSecret())
		// LIST
		api.GET("/records", func(c *gin.Context) {
			c.JSON(http.StatusOK, config.Records)
		})
		// CREATE
		api.POST("/records", func(c *gin.Context) {
			var json NamedRecord
			if c.BindJSON(&json) == nil {
				config.Records[json.Name] = &json.Record
			}
		})
		// SHOW
		api.GET("/records/:address", func(c *gin.Context) {
			c.JSON(http.StatusOK, config.Records[c.Param("address")])
		})
		// UPDATE
		api.PUT("/records/:address", func(c *gin.Context) {
			var json Record
			if c.BindJSON(&json) == nil {
				config.Records[c.Param("address")] = &json
			}
		})
		// DESTROY
		api.DELETE("/records/:address", func(c *gin.Context) {
			delete(config.Records, c.Param("address"))
		})
	}

	router.Run(":8081")
}
