package main

// mssh - Message Stream Shell
//
// Commands:
//
//  = <server>
//
// {command} {name-1}={value-1} {name-2}={value-2}
//
// Translates to:
//
//  {Message Type={command} {name-1}={value-1} {name-2}={value-2}
//
// ? {request} {name-1}={value-1} {name-2}={value-2}
//
//  Send a request: {Request Type={command} {name-1}={value-1} {name-2}={value-2}
//
// To create complex packets:
//
// {command} {name}={value} <{name} {attribute}={value}... {value} >
//

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	ms "github.com/yuvalrakavy/messageStream"
)

type State struct {
	EndPoint        *ms.EndPoint
	Timeout         time.Duration
	Name            string
	Shortcuts       map[string]string
	reader          *bufio.Reader
	pendingShortcut *string
	shortcutNesting int
	connectedTo     string
}

var ErrQuit = fmt.Errorf("Quit")

func parsePacketCommand(endPoint *ms.EndPoint, rawCommandLine string) (*ms.Packet, error) {
	command := strings.Trim(rawCommandLine, " \t")
	isRequest := false

	if command[0] == '?' {
		isRequest = true
		command = strings.Trim(command[1:], " \t")
	}

	tokens := strings.Split(command, " ")
	var packetName string

	if isRequest {
		packetName = "Request"
	} else {
		packetName = "Message"
	}

	element := ms.Element{
		Name:       packetName,
		Attributes: ms.Attributes{},
	}

	if len(tokens) < 1 {
		return nil, fmt.Errorf("Missing command type")
	}

	element.SetAttribute("Type", tokens[0])
	_, err := parseTokens(tokens, 1, &element)

	if err != nil {
		return nil, err
	}

	return &ms.Packet{
		EndPoint: endPoint,
		Element:  &element,
	}, nil
}

func parseTokens(tokens []string, iToken int, element *ms.Element) (int, error) {
	var err error

	for iToken < len(tokens) && tokens[iToken] != ">" {
		token := tokens[iToken]

		if token[0] == '<' {
			var name string

			if len(token) > 1 {
				name = token[1:]
			} else {
				iToken += 1 // Skip '<'
				if iToken >= len(tokens) {
					return -1, fmt.Errorf("Missing child element name after '<' in child element definition")
				}

				name = tokens[iToken]

			}

			iToken += 1 // Skip element name

			childElement := ms.Element{
				Name:       name,
				Attributes: ms.Attributes{},
			}

			iToken, err = parseTokens(tokens, iToken, &childElement)

			if err != nil {
				return -1, err
			}

			if iToken >= len(tokens) || tokens[iToken] != ">" {
				return -1, fmt.Errorf("Missig '>' at end of child element definition")
			}

			element.Children = append(element.Children, childElement)

			iToken += 1
		} else {
			if token[0] != '"' && strings.Contains(token, "=") {
				fields := strings.Split(token, "=")

				element.Attributes[fields[0]] = fields[1]
			} else {
				var value string

				if token[0] == '"' {
					if token[len(token)-1:] != "\"" {
						return -1, fmt.Errorf("Missing \" at end of string literal")
					}
					value = token[1 : len(token)-1]
				} else {
					value = token
				}

				element.Children = append(element.Children, value)
			}

			iToken += 1
		}
	}

	return iToken, nil
}

func (state *State) connect(address string) error {
	if connection, err := net.DialTimeout("tcp", address, 3*time.Second); err != nil {
		return err
	} else {
		if state.EndPoint != nil {
			if err := state.disconnect(); err != nil {
				return err
			}
		}

		state.EndPoint = ms.NewEndPoint(state.Name, connection).OnPacketReceived(func(packet *ms.Packet) {
			fmt.Println("Received: ", packet)
		}).OnClose(func(endPoint *ms.EndPoint) {
			state.EndPoint = nil
		}).Start()

		fmt.Println("Connected to: ", address)

		return nil
	}
}

func (state *State) disconnect() error {
	fmt.Println("Disconnecting from: ", state.EndPoint)

	err := state.EndPoint.Close()
	state.EndPoint = nil
	return err
}

func (state *State) HandleCommand(line string) error {
	tokens := strings.Split(line[1:], " ")

	if len(tokens) < 1 {
		return fmt.Errorf("Missing command after !")
	}

	switch tokens[0][0] {
	case 'q':
		if state.EndPoint != nil {
			_ = state.disconnect()
		}
		return ErrQuit

	case 't':
		if timeout, err := strconv.ParseFloat(tokens[1], 64); err != nil {
			return err
		} else {
			state.Timeout = time.Duration(timeout * float64(time.Second))
			fmt.Println("Request timeout set to ", state.Timeout)
			return nil
		}

	case 'c':
		return state.connect(tokens[1])

	case 'd':
		return state.disconnect()

	case 'n':
		state.Name = tokens[1]
		fmt.Println("Name set to", state.Name)
		return nil

	case 's':
		shortcutTokens := strings.SplitN(line, " ", 3)
		if len(shortcutTokens) == 1 {
			shortcuts := make([]string, 0, len(state.Shortcuts))
			for s := range state.Shortcuts {
				shortcuts = append(shortcuts, s)
			}
			sort.Strings(shortcuts)

			for _, s := range shortcuts {
				fmt.Println(s, "->", state.Shortcuts[s])
			}
		} else if len(shortcutTokens) == 2 {
			definition, found := state.Shortcuts[shortcutTokens[1]]
			if !found {
				fmt.Println("No definition for shortcut named: ", shortcutTokens[1])
			} else {
				fmt.Println("Defined: ", shortcutTokens[1], "->", definition)
			}
		} else {
			state.Shortcuts[shortcutTokens[1]] = shortcutTokens[2]
			fmt.Println("Defined: ", shortcutTokens[1], "->", shortcutTokens[2])
			if err := state.saveShortcuts(); err != nil {
				return err
			}
		}
		return nil

	case 'l':
		if state.EndPoint == nil {
			return fmt.Errorf("Not connected, cannot perform login")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), state.Timeout)
			loginReply := <-state.EndPoint.Request(ms.Attributes{"Type": "_Login", "Name": state.EndPoint.Name}).Submit(ctx)
			cancel()

			if loginReply.Error == nil {
				fmt.Println("Login reply: ", loginReply.Reply)
				state.connectedTo = loginReply.Reply.Element.GetAttribute("Name", "-?-")
			}

			return loginReply.Error
		}

	default:
		return fmt.Errorf("Unsupported command (!quit, !connect, !disconnect, !timeout, !shortcut")
	}
}

func Split(s string) ([]string, error) {
	r := csv.NewReader(strings.NewReader(s))
	r.Comma = ' '
	return r.Read()
}

func (state *State) ExpandShortcut(line string) error {
	var tokens []string

	if _tokens, err := Split(line[1:]); err != nil {
		return err
	} else {
		tokens = _tokens
	}

	showOnly := false

	if len(tokens) < 1 {
		return fmt.Errorf("Missing shortcut name after @")
	}

	shortcutName := tokens[0]
	if shortcutName[0] == '?' {
		showOnly = true
		shortcutName = shortcutName[1:]
	}

	shortcut, found := state.Shortcuts[shortcutName]

	if !found {
		return fmt.Errorf("Shortcut %v is not defined", shortcutName)
	}

	result := ""
	for i := 0; i < len(shortcut); i++ {
		c := shortcut[i]

		if c == '$' && i > 0 && shortcut[i-1] != '\\' && i < len(shortcut)-1 {
			argIndex := int(shortcut[i+1] - '0')
			if argIndex < 1 || argIndex >= len(tokens) {
				return fmt.Errorf("Invalid shortcut argument $%v", argIndex)
			} else {
				i += 1 // Skip the argument number
				result += tokens[argIndex]
			}
		} else {
			result += string(c)
		}
	}

	if showOnly {
		fmt.Println("?-> ", result)
		return nil
	} else {
		return state.PushInput(result)
	}
}

func (state *State) Execute(line string) error {

	if len(line) > 0 {
		if line[0] == '!' {
			if err := state.HandleCommand(line); err != nil {
				return err
			}
		} else if line[0] == '@' {
			if err := state.ExpandShortcut(line); err != nil {
				return err
			}
		} else {
			if state.EndPoint != nil {
				packet, err := parsePacketCommand(state.EndPoint, line)

				if err == nil {
					if packet.Element.Name != "Request" {
						fmt.Println("Sending: ", packet)
						err = packet.Send()
					} else {
						fmt.Println("Submitting: ", packet)
						ctx, cancelFunc := context.WithTimeout(context.Background(), state.Timeout)
						submitResult := <-packet.Submit(ctx)
						cancelFunc()

						err = submitResult.Error
						if err == nil {
							fmt.Println("Got reply: ", submitResult.Reply)
						}
					}
				}

				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("You cannot send/submit a packet when disconnected, use !connect <address> command to connect to service")
			}
		}

	}

	return nil
}

func (state *State) GetInput() (string, error) {
	if state.pendingShortcut == nil {
		state.shortcutNesting = 0
		endPointName := "--Not open--"

		if state.EndPoint != nil {
			endPointName = state.EndPoint.Name
		}

		fmt.Print("[", endPointName, " -> ", state.connectedTo, "]: ")

		if line, err := state.reader.ReadString('\n'); err != nil {
			return "", err
		} else {
			line = strings.TrimSuffix(line, "\n")
			return line, nil
		}
	} else {
		line := *state.pendingShortcut
		state.pendingShortcut = nil

		fmt.Println("-> ", line)
		return line, nil
	}
}

func (state *State) PushInput(line string) error {
	state.shortcutNesting += 1

	if state.shortcutNesting > 10 {
		state.pendingShortcut = nil
		return fmt.Errorf("Shortcut nesting is too deep, you probably have recursive shortcut")
	}

	state.pendingShortcut = &line
	return nil
}

func (state *State) loadShortcuts() error {
	bytes, err := ioutil.ReadFile("mssh-shortcuts.txt")

	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}

	content := string(bytes)
	lines := strings.Split(content, "\n")

	// Clear all existing shortcuts
	state.Shortcuts = make(map[string]string)

	for _, line := range lines {
		if len(line) > 0 {
			shortcut := strings.SplitN(line, ":", 2)

			if len(shortcut) < 2 {
				return fmt.Errorf("Invalid shortcut definition: %v (should be <name>:<definition>)\n", line)
			}

			state.Shortcuts[shortcut[0]] = shortcut[1]
		}
	}

	return nil
}

func (state *State) saveShortcuts() error {
	if f, err := os.Create("mssh-shortcuts.txt"); err == nil || os.IsNotExist(err) {
		for name, definition := range state.Shortcuts {
			_, _ = f.WriteString(fmt.Sprintf("%v:%v\n", name, definition))
		}

		f.Close()
		return nil
	} else {
		return err
	}
}

func main() {
	state := State{
		EndPoint:    nil,
		Timeout:     2 * time.Second,
		Name:        "mssh",
		Shortcuts:   make(map[string]string),
		reader:      bufio.NewReader(os.Stdin),
		connectedTo: "-?-",
	}

	if err := state.loadShortcuts(); err != nil {
		fmt.Println("Error loading shortcuts:", err.Error())
	}

	for {
		var err error
		var line string

		if line, err = state.GetInput(); err == nil {
			err = state.Execute(line)

			if err == ErrQuit {
				break
			}
		}

		if err != nil {
			fmt.Println("Error: ", err.Error())
		}
	}
}
