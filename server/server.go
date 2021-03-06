package server

import (
	"fmt"
    "net"
    "os"
    "crypto/rsa" 
    "crypto/rand" 
    "encoding/pem"
    "encoding/binary"
    "crypto/x509" 

	"golang.org/x/crypto/ssh"
    "dev.justinjudd.org/justin/easyssh"
    "github.com/microstacks/stack/endpoint/omap"
    "github.com/microstacks/stack/endpoint/utils"
    "github.com/prometheus/common/log"
)

var rfwds map[string]net.Listener = make(map[string]net.Listener)

type Callback func(*omap.OMap, *utils.Host)

const (
	RemoteForwardRequest       = "tcpip-forward"        
	ForwardedTCPReturnRequest  = "forwarded-tcpip"      
	CancelRemoteForwardRequest = "cancel-tcpip-forward" 
)

// tcpipForward is structure for RFC 4254 7.1 "tcpip-forward" request
type tcpipForward struct {
	Host string
	Port uint32
}

// directForward is struxture for RFC 4254 7.2 - can be used for "forwarded-tcpip" and "direct-tcpip"
type directForward struct {
	Host1 string
	Port1 uint32
	Host2 string
	Port2 uint32
}

/*
 * User record
 */
type user struct {
    user string
    ccb Callback
    dcb Callback
    m   *omap.OMap
}

/*
 * User DB
 */
var userDB map[string]user = make(map[string]user, 1)

/*
 * MakeSSHKeyPair make a pair of public and private keys for SSH access.
 * Public key is encoded in the format for inclusion in an OpenSSH authorized_keys file.
 * Private Key generated is PEM encoded
 */
func MakeSSHKeyPair() ([]byte, []byte, error) {
    privateKey, err := rsa.GenerateKey(rand.Reader, 1024)
    if err != nil {
        return nil, nil, err
    }

    privateKeyBlock := pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}
    privateKeyPEM := pem.EncodeToMemory(&privateKeyBlock)
    
    // generate and write public key
    pub, err := ssh.NewPublicKey(&privateKey.PublicKey)
    if err != nil {
        return nil, nil, err
    }
    
    return privateKeyPEM, ssh.MarshalAuthorizedKey(pub), nil
}


// TCPIPForwardRequest fulfills RFC 4254 7.1 "tcpip-forward" request
//
// TODO: Need to add state to handle "cancel-tcpip-forward"
func TCPIPForwardRequest(req *ssh.Request, sshConn ssh.Conn) {

	t := tcpipForward{}
	reply := (t.Port == 0) && req.WantReply
	ssh.Unmarshal(req.Payload, &t)
	addr := fmt.Sprintf("%s:%d", t.Host, t.Port)
    
	ln, err := net.Listen("tcp", addr) //tie to the client connection
	if err != nil {
		log.Debug("Unable to listen on address: ", addr)
		return
	}
    
    //Store in listener map
    rfwds[addr] = ln
    
    fmt.Println("SSH Server: Remote Port Forward request for ", ln.Addr().String(), 
                " from ", sshConn.RemoteAddr().String())

	quit := make(chan bool)

	if reply { // Client sent port 0. let them know which port is actually being used

		_, port, err := utils.GetHostPort(ln.Addr())
		if err != nil {
			return
		}

		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(port))
		t.Port = uint32(port)
		req.Reply(true, b)
	} else {
		req.Reply(true, nil)
	}

    u := userDB[sshConn.User()]
    h := &utils.Host{}
    h.LocalIP = t.Host
    h.LocalPort = t.Port
    tcpAddr, _ := sshConn.RemoteAddr().(*net.TCPAddr)
    h.RemoteIP = tcpAddr.IP.String()
    h.RemotePort = t.Port

    go u.ccb(u.m, h)    
    
	go func() { // Handle incoming connections on this new listener
		for {
			select {
			case <-quit:

				return
			default:
				conn, err := ln.Accept()
				if err != nil { // Unable to accept new connection - listener likely closed
					continue
				}

                fmt.Println("SSH Server: New Connection request local port ", conn.LocalAddr())
    
                go func(conn net.Conn) {
					p := directForward{}
					var err error

					var portnum int
					p.Host1 = t.Host
					p.Port1 = t.Port
					p.Host2, portnum, err = utils.GetHostPort(conn.RemoteAddr())
					if err != nil {
                        fmt.Println(err)
						return
					}

                    p.Host2 = os.Getenv("BINDADDR")

					p.Port2 = uint32(portnum)
					ch, reqs, err := sshConn.OpenChannel(ForwardedTCPReturnRequest, ssh.Marshal(p))
					if err != nil {
						log.Debug("Open forwarded Channel: ", err.Error())
						return
					}
					go ssh.DiscardRequests(reqs)

                    fmt.Println("SSH Server: Routing Data between ", conn.RemoteAddr(), "<-->", conn.LocalAddr(), "@", sshConn.RemoteAddr().String())
					go func(ch ssh.Channel, conn net.Conn) {

						close := func() {
							ch.Close()
							conn.Close()

							log.Debug("forwarding closed")
						}

						go utils.CopyReadWriters(conn, ch, close)

					}(ch, conn)

				}(conn)
			}

		}

	}()
    
    sshConn.Wait()
    u = userDB[sshConn.User()]
    go u.dcb(u.m, h)    
    
    log.Debug("Stop forwarding/listening on ", ln.Addr())
    ln.Close()
    quit <- true        

}

func TCPIPCancelRequest(req *ssh.Request, sshConn ssh.Conn) {
	t := tcpipForward{}
	reply := (t.Port == 0) && req.WantReply
	ssh.Unmarshal(req.Payload, &t)
	addr := fmt.Sprintf("%s:%d", t.Host, t.Port)
    
    ln, ok := rfwds[addr]
    
    if ok {
        if reply { // Client sent port 0. let them know which port is actually being used
            _, port, err := utils.GetHostPort(ln.Addr())
            if err != nil {
                return
            }

            b := make([]byte, 4)
            binary.BigEndian.PutUint32(b, uint32(port))
            t.Port = uint32(port)
            req.Reply(true, b)
        } else {
            req.Reply(true, nil)
        }

        ln.Close()
        delete(rfwds, addr)        
    }
}

func Listen() (error) {

    // Handle Authentication
    config := &ssh.ServerConfig{
        PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
            log.Debug("C.User()=", c.User(), "Users=", userDB)
            if u, ok := userDB[c.User()]; ok && string(pass) == "123456789" {
                return &ssh.Permissions{Extensions: map[string]string{"user": u.user}}, nil
            }
            return nil, fmt.Errorf("password rejected for %q", c.User())
        },
    }

    priv, _, err := MakeSSHKeyPair()

    private, err := ssh.ParsePrivateKey(priv)
    if err != nil {
        log.Error("Failed to parse private key: ", err)
    }

    config.AddHostKey(private)
        
    easyssh.EnableLogging(os.Stderr)
    easyssh.HandleChannel(easyssh.SessionRequest, easyssh.SessionHandler())
	easyssh.HandleChannel(easyssh.DirectForwardRequest, easyssh.DirectPortForwardHandler())
	easyssh.HandleRequestFunc(easyssh.RemoteForwardRequest, easyssh.GlobalRequestHandlerFunc(TCPIPForwardRequest))
	easyssh.HandleRequestFunc(easyssh.CancelRemoteForwardRequest, easyssh.GlobalRequestHandlerFunc(TCPIPCancelRequest))
    
    // Listen & Accept connections
    easyssh.ListenAndServe(":22", config, nil)
    return nil
}

func AddUser(uname string, m *omap.OMap, ccb Callback, dcb Callback) {
    u := user{}
    u.user = uname
    u.m = m
    u.ccb = ccb
    u.dcb = dcb
    
    log.Debug("Adding User=", u)
    
    // Add user to database
    userDB[uname] = u
    
    log.Debug("Users=", userDB)
}