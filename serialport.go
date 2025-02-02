package main

import (
	"bytes"
	"encoding/json"
	"github.com/johnlauer/goserial"
	//"github.com/facchinm/go-serial"
	"io"
	"log"
	"strconv"
	"strings"
	"time"
)

type SerialConfig struct {
	Name string
	Baud int

	// Size     int // 0 get translated to 8
	// Parity   SomeNewTypeToGetCorrectDefaultOf_None
	// StopBits SomeNewTypeToGetCorrectDefaultOf_1

	// RTSFlowControl bool
	// DTRFlowControl bool
	// XONFlowControl bool

	// CRLFTranslate bool
	// TimeoutStuff int
	RtsOn bool
	DtrOn bool
}

type serport struct {
	// The serial port connection.

	// Needed for original serial library
	portConf *serial.Config
	// Needed for Arduino serial library
	//portConf *SerialConfig

	portIo io.ReadWriteCloser

	done chan bool // signals the end of this request

	// Keep track of whether we're being actively closed
	// just so we don't show scary error messages
	isClosing bool

	// counter incremented on queue, decremented on write
	itemsInBuffer int

	// buffered channel containing up to 25600 outbound messages.
	sendBuffered chan Cmd

	// unbuffered channel of outbound messages that bypass internal serial port buffer
	sendNoBuf chan Cmd

	// Do we have an extra channel/thread to watch our buffer?
	BufferType string
	//bufferwatcher *BufferflowDummypause
	bufferwatcher Bufferflow

	// Keep track of whether this is the primary serial port, i.e. cnc controller
	// or if its secondary, i.e. a backup port or arduino or something tertiary
	IsPrimary   bool
	IsSecondary bool

	// Feedrate override value
	feedRateOverride float32
}

type Cmd struct {
	data                       string
	id                         string
	skippedBuffer              bool
	willHandleCompleteResponse bool
}

type CmdComplete struct {
	Cmd     string
	Id      string
	P       string
	BufSize int    `json:"-"`
	D       string `json:"-"`
}

type qwReport struct {
	Cmd  string
	QCnt int
	Id   string
	D    string `json:"-"`
	Buf  string `json:"-"`
	P    string
}

type qwReportWithData struct {
	Cmd  string
	QCnt int
	Id   string
	D    string //`json:"-"`
	Buf  string `json:"-"`
	P    string
}

type SpPortMessage struct {
	P string // the port, i.e. com22
	D string // the data, i.e. G0 X0 Y0
}

func (p *serport) reader() {

	//var buf bytes.Buffer
	ch := make([]byte, 1024)
	timeCheckOpen := time.Now()

	for {

		n, err := p.portIo.Read(ch)

		//if we detect that port is closing, break out o this for{} loop.
		if p.isClosing {
			strmsg := "Shutting down reader on " + p.portConf.Name
			log.Println(strmsg)
			h.broadcastSys <- []byte(strmsg)
			break
		}

		// read can return legitimate bytes as well as an error
		// so process the bytes if n > 0
		if n > 0 {
			//log.Print("Read " + strconv.Itoa(n) + " bytes ch: " + string(ch))
			data := string(ch[:n])
			//log.Print("The data i will convert to json is:")
			//log.Print(data)

			// give the data to our bufferflow so it can do it's work
			// to read/translate the data to see if it wants to block
			// writes to the serialport. each bufferflow type will decide
			// this on its own based on its logic, i.e. tinyg vs grbl vs others
			//p.b.bufferwatcher..OnIncomingData(data)
			p.bufferwatcher.OnIncomingData(data)

			// see if the OnIncomingData handled the broadcast back
			// to the user. this option was added in case the OnIncomingData wanted
			// to do something fancier or implementation specific, i.e. TinyG Buffer
			// actually sends back data on a perline basis rather than our method
			// where we just send the moment we get it. the reason for this is that
			// the browser was sometimes getting back packets out of order which
			// of course would screw things up when parsing

			if p.bufferwatcher.IsBufferGloballySendingBackIncomingData() == false {
				//m := SpPortMessage{"Alice", "Hello"}
				m := SpPortMessage{p.portConf.Name, data}
				//log.Print("The m obj struct is:")
				//log.Print(m)

				//b, err := json.MarshalIndent(m, "", "\t")
				b, err := json.Marshal(m)
				if err != nil {
					log.Println(err)
					h.broadcastSys <- []byte("Error creating json on " + p.portConf.Name + " " +
						err.Error() + " The data we were trying to convert is: " + string(ch[:n]))
					break
				}
				//log.Print("Printing out json byte data...")
				//log.Print(string(b))
				h.broadcastSys <- b
				//h.broadcastSys <- []byte("{ \"p\" : \"" + p.portConf.Name + "\", \"d\": \"" + string(ch[:n]) + "\" }\n")
			}
		}

		// double check that we got characters in the buffer
		// before deciding if an EOF is legitimately a reason
		// to close the port because we're seeing that on some
		// os's like Linux/Ubuntu you get an EOF when you open
		// the port. Perhaps the EOF was buffered from a previous
		// close and the OS doesn't clear out that buffer on a new
		// connect. This means we'll only catch EOF's when there are
		// other characters with it, but that seems to work ok
		if n <= 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				// hit end of file
				log.Println("Hit end of file on serial port")
				h.broadcastSys <- []byte("{\"Cmd\":\"OpenFail\",\"Desc\":\"Got EOF (End of File) on port which usually means another app other than Serial Port JSON Server is locking your port. " + err.Error() + "\",\"Port\":\"" + p.portConf.Name + "\",\"Baud\":" + strconv.Itoa(p.portConf.Baud) + "}")

			}

			if err != nil {
				log.Println(err)
				h.broadcastSys <- []byte("Error reading on " + p.portConf.Name + " " +
					err.Error() + " Closing port.")
				h.broadcastSys <- []byte("{\"Cmd\":\"OpenFail\",\"Desc\":\"Got error reading on port. " + err.Error() + "\",\"Port\":\"" + p.portConf.Name + "\",\"Baud\":" + strconv.Itoa(p.portConf.Baud) + "}")
				break
			}

			// Keep track of time difference between two consecutive read with n == 0 and err == nil
			// we get here if the port has been disconnected while open (cpu usage will jump to 100%)
			// let's close the port only if the events are extremely fast (<1ms)
			if err == nil {
				diff := time.Since(timeCheckOpen)
				if diff.Nanoseconds() < 1000000 {
					p.isClosing = true
				}
				timeCheckOpen = time.Now()
			}
		}
	}
	p.portIo.Close()
}

// this method runs as its own thread because it's instantiated
// as a "go" method. so if it blocks inside, it is ok
func (p *serport) writerBuffered() {

	// this method can panic if user closes serial port and something is
	// in BlockUntilReady() and then a send occurs on p.sendNoBuf

	defer func() {
		if e := recover(); e != nil {
			// e is the interface{} typed-value we passed to panic()
			log.Println("Got panic: ", e) // Prints "Whoops: boom!"
		}
	}()

	// this for loop blocks on p.sendBuffered until that channel
	// sees something come in
	for data := range p.sendBuffered {

		//log.Printf("Got p.sendBuffered. data:%v, id:%v\n", string(data.data), string(data.id))

		// we want to block here if we are being asked
		// to pause.
		goodToGo, willHandleCompleteResponse := p.bufferwatcher.BlockUntilReady(string(data.data), data.id)

		if goodToGo == false {
			log.Println("We got back from BlockUntilReady() but apparently we must cancel this cmd")
			// since we won't get a buffer decrement in p.sendNoBuf, we must do it here
			p.itemsInBuffer--
		} else {
			// send to the non-buffered serial port writer
			//log.Println("About to send to p.sendNoBuf channel")
			data.willHandleCompleteResponse = willHandleCompleteResponse
			p.sendNoBuf <- data
		}
	}
	msgstr := "writerBuffered just got closed. make sure you make a new one. port:" + p.portConf.Name
	log.Println(msgstr)
	h.broadcastSys <- []byte(msgstr)
}

// this method runs as its own thread because it's instantiated
// as a "go" method. so if it blocks inside, it is ok
func (p *serport) writerNoBuf() {
	// this for loop blocks on p.send until that channel
	// sees something come in
	for data := range p.sendNoBuf {

		log.Printf("Got p.sendNoBuf. id:%v, data:%v\n", string(data.id), strings.Replace(string(data.data), "\n", "\\n", -1))

		// if we get here, we were able to write successfully
		// to the serial port because it blocks until it can write

		// decrement counter
		p.itemsInBuffer--
		log.Printf("itemsInBuffer:%v\n", p.itemsInBuffer)
		//h.broadcastSys <- []byte("{\"Cmd\":\"Write\",\"QCnt\":" + strconv.Itoa(p.itemsInBuffer) + ",\"Byte\":" + strconv.Itoa(n2) + ",\"Port\":\"" + p.portConf.Name + "\"}")

		// Figure out buffered or not buffered
		buf := "Buf"
		if data.skippedBuffer {
			buf = "NoBuf"
		}

		// WARNING - Feedrate Override doesn't really belong in here because this is supposed
		// to be a generic implementation of sending/receiving to serial ports
		// However, there's not really a better place to put this because you need to know
		// last minute what the feedrate override is and let the user adjust it at any time
		// If you want a generic serial port implementation, remove this last minute call from this code
		didWeOverride, newData := doFeedRateOverride(data.data, p.feedRateOverride)

		if didWeOverride {
			// We need to reset the gcode and make the qwReport be what we want
			// Since we changed the gcode, we need to report it back to the user
			// For reducing load on websocket, stop transmitting write data
			data.data = newData
			qwr := qwReportWithData{
				Cmd:  "Write",
				QCnt: p.itemsInBuffer,
				Id:   string(data.id),
				D:    string(data.data),
				Buf:  buf,
				P:    p.portConf.Name,
			}
			qwrJson, _ := json.Marshal(qwr)
			h.broadcastSys <- qwrJson
		} else {
			// For reducing load on websocket, stop transmitting write data
			qwr := qwReport{
				Cmd:  "Write",
				QCnt: p.itemsInBuffer,
				Id:   string(data.id),
				D:    string(data.data),
				Buf:  buf,
				P:    p.portConf.Name,
			}
			qwrJson, _ := json.Marshal(qwr)
			h.broadcastSys <- qwrJson
		}

		// FINALLY, OF ALL THE CODE IN THIS PROJECT
		// WE TRULY/FINALLY GET TO WRITE TO THE SERIAL PORT!
		_, err := p.portIo.Write([]byte(data.data)) // n2, err :=

		// see if we need to send back the completeResponse
		if data.willHandleCompleteResponse == false {
			// we need to send back complete response
			// Send fake cmd:"Complete" back
			//strCmd := data.data
			m := CmdComplete{"CompleteFake", data.id, p.portConf.Name, -1, data.data}
			msgJson, err := json.Marshal(m)
			if err == nil {
				h.broadcastSys <- msgJson
			}

		}

		//log.Print("Just wrote ", n2, " bytes to serial: ", string(data.data))
		//log.Print(n2)
		//log.Print(" bytes to serial: ")
		//log.Print(data)
		if err != nil {
			errstr := "Error writing to " + p.portConf.Name + " " + err.Error() + " Closing port."
			log.Print(errstr)
			h.broadcastSys <- []byte(errstr)
			break
		}
	}
	msgstr := "Shutting down writer on " + p.portConf.Name
	log.Println(msgstr)
	h.broadcastSys <- []byte(msgstr)
	p.portIo.Close()
}

func spHandlerOpen(portname string, baud int, buftype string, isSecondary bool) {

	log.Print("Inside spHandler")

	var out bytes.Buffer

	out.WriteString("Opening serial port ")
	out.WriteString(portname)
	out.WriteString(" at ")
	out.WriteString(strconv.Itoa(baud))
	out.WriteString(" baud")
	log.Print(out.String())

	//h.broadcast <- []byte("Opened a serial port ")
	//h.broadcastSys <- out.Bytes()

	isPrimary := true
	if isSecondary {
		isPrimary = false
	}

	//options := serial.RawOptions
	//options.BitRate = 1200
	//options.FlowControl = serial.FLOWCONTROL_RTSCTS
	//p, err := options.Open(portname)

	// Needed for original serial library
	conf := &serial.Config{}
	conf.Baud = baud
	conf.Name = portname
	conf.RtsOn = true
	conf.DtrOn = false

	// Needed for Arduino serial library
	/*
		conf := &SerialConfig{Name: portname, Baud: baud, RtsOn: true}
		conf.DtrOn = false
	*/

	/*
		// Needed for Arduino serial library
		mode := &serial.Mode{
			BaudRate: baud,
			Vmin:     0,
			Vtimeout: 10,
		}
	*/
	//mode.DataBits = 7
	//mode.Parity = 0
	//mode.StopBits = 1

	// Needed for original serial library
	sp, err := serial.OpenPort(conf)
	// Needed for Arduino serial library
	//sp, err := serial.OpenPort(portname, mode)

	log.Print("Just tried to open port")
	if err != nil {
		//log.Fatal(err)
		log.Print("Error opening port " + err.Error())
		//h.broadcastSys <- []byte("Error opening port. " + err.Error())
		h.broadcastSys <- []byte("{\"Cmd\":\"OpenFail\",\"Desc\":\"Error opening port. " + err.Error() + "\",\"Port\":\"" + conf.Name + "\",\"Baud\":" + strconv.Itoa(conf.Baud) + "}")

		return
	}
	log.Print("Opened port successfully")
	//p := &serport{send: make(chan []byte, 256), portConf: conf, portIo: sp}
	// we can go up to 500,000 lines of gcode in the buffer
	p := &serport{sendBuffered: make(chan Cmd, 500000), sendNoBuf: make(chan Cmd), portConf: conf, portIo: sp, BufferType: buftype, IsPrimary: isPrimary, IsSecondary: isSecondary}

	// if user asked for a buffer watcher, i.e. tinyg/grbl then attach here
	if buftype == "tinyg" {

		bw := &BufferflowTinyg{Name: "tinyg", parent_serport: p}
		bw.Init()
		bw.Port = portname
		p.bufferwatcher = bw
	} else if buftype == "tinygg2" {

		bw := &BufferflowTinygG2{Name: "tinygg2", parent_serport: p}
		bw.Init()
		bw.Port = portname
		p.bufferwatcher = bw
	} else if buftype == "tinyg_linemode" {

		bw := &BufferflowTinygPktMode{Name: "tinyg_linemode", parent_serport: p}
		bw.Init()
		bw.Port = portname
		p.bufferwatcher = bw

	} else if buftype == "dummypause" {

		// this is a dummy pause type bufferflow object
		// to test artificially a delay on the serial port write
		// it just pauses 3 seconds on each serial port write
		bw := &BufferflowDummypause{}
		bw.Init()
		bw.Port = portname
		p.bufferwatcher = bw
	} else if buftype == "grbl" {
		// grbl bufferflow
		// store port as parent_serport for use in intializing a status query loop for '?'
		bw := &BufferflowGrbl{Name: "grbl", parent_serport: p}
		bw.Init()
		bw.Port = portname
		p.bufferwatcher = bw
	} else if buftype == "marlin" {
		// marlin bufferflow
		// store port as parent_serport for use in intializing a status query loop for '?'
		bw := &BufferflowMarlin{Name: "marlin", parent_serport: p}
		bw.Init()
		bw.Port = portname
		p.bufferwatcher = bw
	} else {
		bw := &BufferflowDefault{}
		bw.Init()
		bw.Port = portname
		p.bufferwatcher = bw
	}

	sh.register <- p
	defer func() { sh.unregister <- p }()
	// this is internally buffered thread to not send to serial port if blocked
	go p.writerBuffered()
	// this is thread to send to serial port regardless of block
	go p.writerNoBuf()
	p.reader()
	//go p.reader()
	//p.done = make(chan bool)
	//<-p.done
}

func spHandlerCloseExperimental(p *serport) {
	h.broadcastSys <- []byte("Pre-closing serial port " + p.portConf.Name)
	p.isClosing = true
	//close the port

	p.bufferwatcher.Close()
	p.portIo.Close()
	h.broadcastSys <- []byte("Bufferwatcher closed")
	p.portIo.Close()
	//elicit response from hardware to close out p.reader()
	//_, _ = p.portIo.Write([]byte("?"))
	//p.portIo.Read(nil)

	//close(p.portIo)
	h.broadcastSys <- []byte("portIo closed")
	close(p.sendBuffered)
	h.broadcastSys <- []byte("p.sendBuffered closed")
	close(p.sendNoBuf)
	h.broadcastSys <- []byte("p.sendNoBuf closed")

	//p.done <- true

	// unregister myself
	// we already have a deferred unregister in place from when
	// we opened. the only thing holding up that thread is the p.reader()
	// so if we close the reader we should get an exit
	h.broadcastSys <- []byte("Closing serial port " + p.portConf.Name)
}

func spHandlerClose(p *serport) {
	p.isClosing = true
	//close the port
	//elicit response from hardware to close out p.reader()
	_, _ = p.portIo.Write([]byte("?"))

	p.bufferwatcher.Close()
	p.portIo.Close()
	// unregister myself
	// we already have a deferred unregister in place from when
	// we opened. the only thing holding up that thread is the p.reader()
	// so if we close the reader we should get an exit
	h.broadcastSys <- []byte("Closing serial port " + p.portConf.Name)
}
