/* Copyright (C) Ashish Thakwani - All Rights Reserved
 * Unauthorized copying of this file, via any medium is strictly prohibited
 * Proprietary and confidential
 * Written by Ashish Thakwani <athakwani@gmail.com>, August 2017
 */
package register

import (
    "fmt"
    "net"
    "regexp"
    "errors"
    "time"
    "net/rpc"
    "net/http"
    
    "github.com/duppercloud/trafficrouter/utils"
    "github.com/duppercloud/trafficrouter/ssh"
    log "github.com/Sirupsen/logrus"
)

/*
 *  opt struct
 */
type Reg struct {
    opt   string               //option string
    Lhost string               //local hostname
    Lport string               //local port to connect
    Rhost string               //remote host to connect to
    Rport string               //remote host port
    user string                //remote username
}

var goroutines map[string]chan bool = make(map[string]chan bool, 1)

type Args struct {
    Lport string
    Rport string
}

type RPC struct {
    opts []string
    passwd   string
    interval int
    debug    bool
}


/*
 *  forEach parser callback
 */
type parsecb func(*Reg) error

func Cleanup() {
    for _, done := range goroutines {
        if done != nil {
            close(done)
        }
    }
}

/*
 *  --Regiser option parser logic
 */
func parse(str string) (string, string, string, string) {
    var expr = regexp.MustCompile(`([a-zA-Z^:][a-zA-Z0-9\-\.]+):([0-9]+|\*)(@([^:]+)(:([0-9]+))?)?$`)
	parts := expr.FindStringSubmatch(str)
	if len(parts) == 0 {
        utils.Check(errors.New(fmt.Sprintf("Option parse error: [%s]. Format lhost:lport[@rhost:rport]\n", str)))
	}
    
    if parts[6] == "" {
        parts[6] = parts[2]
    }
    
    return parts[1], parts[2], parts[4], parts[6]
}

/*
 *  -regiser options iterater
 */
func forEach(opts []string, cb parsecb) error {
    for _, opt := range opts {

        r := Reg{opt: opt}
        r.Lhost, r.Lport, r.Rhost, r.Rport = parse(opt)

        if r.Lport == "*" {
            r.user = r.Lhost
        } else {
            r.user = r.Lhost + "." + r.Lport
        }

        log.Debug("laddr=", r.Lhost, ",",
                  "lport=", r.Lport, ",",
                  "rhost=", r.Rhost, ",",
                  "rport=", r.Rport, ",",
                  "ruser=", r.user)
        
        if err := cb(&r); err != nil {
            return err
        }
    }
    return nil
}


func (r Reg) reconnect(passwd string, interval int, debug bool) {
    // Channel to notify when to stop this go routine
    done := make(chan bool)
    goroutines[r.Lport] = done
    
    for {      
        
        // Go connect, ignore errors and keep retrying
        r.connect(passwd, debug)
        
        // Diconnect all ssh connection if channel is closed and return.
        select {
            case _, ok := <- done:
                log.Debug("Terminating goroutine")
                if !ok {
                    // Check if host exists
                    ipArr, _ := net.LookupHost(r.Rhost)

                    for _, ip := range ipArr {
                        hash := r.Lhost + "." + r.Lport + "@" + ip
                        ssh.Disconnect(hash)
                    }
                    return
                }
            case  <- time.After( time.Duration(interval) * 1000 * time.Millisecond):
            /* no-op */
        }
    }
}


/*
 *  Connect internal to remote host and periodically check the state.
 */
func (r Reg) connect(passwd string, debug bool) error {

    // Check if host exists
    ipArr, err := net.LookupHost(r.Rhost)
    if err != nil {
        return err
    }

    // Connect to all IP address for remote host
    for _, ip := range ipArr {
        hash := r.Lhost + "." + r.Lport + "@" + ip

        // flag for skipping self connection
        skip := false

        // Make sure to not connect to itself for container:* scenario
        laddrs, _ := net.InterfaceAddrs()
        for _, address := range laddrs {
            if ipnet, ok := address.(*net.IPNet); ok {
                if ipnet.IP.To4() != nil {
                    if ip == ipnet.IP.String() {
                        skip = true
                        break
                    }
                }
            }                
        }

        // Skip if remote IP is one of local interface ip
        if skip {
            continue 
        }

        // connect to dynamic port.
        // store assigned port in map
        // Use the same port for rest of the connections.
        if !ssh.IsConnected(hash) {
            fmt.Println("Connecting...", hash)
            r.Rport, err = ssh.Connect(r.user, passwd, ip, r.Lport, r.Rport, hash, debug)
            if err != nil {
                return err
            }
        }            
    }
    
    return nil
}        

/*
 *  Connect to remote host and periodically check the state.
 */
func (r Reg) Connect(passwd string, interval int, debug bool) error {

    err := r.connect(passwd, debug)
    if err != nil {
        go r.reconnect(passwd, interval, debug)
    }
    
    return err
}

/*
 *  Connect/Disconnect changes based on portmap
 */
func (r Reg) Disconnect() {
    // Disconnect all connections for LPORT by closing goroutine channel.
    if goroutines[r.Lport] != nil {
        close(goroutines[r.Lport])
        delete(goroutines, r.Lport)                
    }
}


/*
 *  Connect to all hosts
 */
func (_rpc RPC) Connect(args *Args, errno *int) error {

    regs := []Reg{}
    
    log.Debug("RPC Connect invoked with args=", args)
    // Start event loop for each option
    forEach(_rpc.opts, func(r *Reg) error {
        r.Lport = args.Lport
        r.Rport = args.Rport
        if err := r.Connect(_rpc.passwd, _rpc.interval, _rpc.debug); err != nil{
            log.Error(err)
            for _, reg := range regs {
                reg.Disconnect()
            }
            *errno = -1
            return err
        }
        regs = append(regs, *r)
        return nil
    })    
        
    *errno = 0
    return nil
}

/*
 *  Connect to all hosts
 */
func (_rpc RPC) Disconnect(args *Args, errno *int) error {    
    log.Debug("RPC Disconnect invoked with args=", args)
    // Start event loop for each option
    forEach(_rpc.opts, func(r *Reg) error {
        r.Lport = args.Lport
        r.Rport = args.Rport
        r.Disconnect()
        return nil
    })    
        
    *errno = 0
    return nil
}

/*
 *  Process --regiser options
 */
func Process(passwd string, opts []string, count int, interval int, debug bool) {
    log.Debug(opts)

    // Init RPC struct and export for remote calling
    _rpc := new(RPC)
    _rpc.opts = opts
    _rpc.passwd = passwd
    _rpc.interval = interval
    _rpc.debug = debug
    
    rpc.Register(_rpc)
    rpc.HandleHTTP()
    l, e := net.Listen("tcp", "localhost:3877")
    if e != nil {
        log.Error("listen error:", e)
    }
    go http.Serve(l, nil)    
    
    
    // Start event loop for each option
    forEach(opts, func(r *Reg) error {            
        if r.Lport != "*" {
            if err := r.Connect(passwd, interval, debug); err != nil{
                log.Error(err)
            }
        }
        return nil
    })      
    
}
