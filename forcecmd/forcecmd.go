/* Copyright 2017, Ashish Thakwani. 
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.LICENSE file.
 */
package forcecmd

import (
    "fmt"
    "encoding/json"
    "os"
    "regexp"
    "strconv"
    "net"
    "log"
    "os/signal"
    "syscall"

    "../utils"
    "golang.org/x/sys/unix"
    netutil "github.com/shirou/gopsutil/net"
    ps "github.com/shirou/gopsutil/process"
    
)

/*
 * Wait for parent to exit.
 */
func waitForExit(fd int) {

    sigs := make(chan os.Signal, 1)    
    signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
    
    go func() {
        sig := <-sigs
        fmt.Println(sig)
        utils.UnlockFile(fd)
        os.Exit(1)
    }()

    flag := unix.SIGHUP
    if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(flag), 0, 0, 0); err != nil {
        return
    }
    
    utils.BlockForever()
}

/*
 *  Get Parent process's pid and commandline.
 */
func getProcParam(p int32) (*ps.Process, string) {

    // Init process struct based on PID
    proc, err := ps.NewProcess(p)
    utils.Check(err)

    // Get command line of parent process.
    cmd, err := proc.Cmdline()
    utils.Check(err)
    
    return proc, cmd
}

/*
 *  Get tunnel connections parameters in host struct
 */
func getConnParams(pid int32, h *utils.Host) {
    // Get SSH reverse tunnel connection information.
    // 3 sockers are opened by ssh:
    // 1. Connection from client to server
    // 2. Listening socket for IPv4
    // 3. Listening socket for IPv6
    conns, err := netutil.ConnectionsPid("inet", pid)
    utils.Check(err)
    log.Println(conns)

    for _, c := range conns {
        // Family = 2 indicates IPv4 socket. Store Listen Port
        // in host structure.
        if c.Family == 2 && c.Status == "LISTEN" {
            h.ListenPort = c.Laddr.Port
        }

        // Store Established connection IP & Port in host structure.
        if c.Family == 2 && c.Status == "ESTABLISHED" {
            h.RemoteIP   = c.Raddr.IP
            h.RemotePort = c.Raddr.Port
        }
    }
}


/*
 *  Get Client configuration parameters in host struct
 */
func getConfigParams(h *utils.Host) {
    
    // Get Client config which should be the last argument
    cfgstr := os.Args[len(os.Args) - 1]
    log.Println(cfgstr)
    
    // Conver config to json
    cfg := utils.Config{}
    json.Unmarshal([]byte(cfgstr), &cfg)
    
    // Update and log host var
    h.AppPort = cfg.Port                
    h.Config = cfg
    h.Uid = os.Getuid()
    
}

/*
 *  Match string with regex.
 */
func match(regex string, str string) bool {

    if len(str) > 0 {
        // find server with current user ID using command line match 
        ok, err := regexp.MatchString(regex, str)
        utils.Check(err)

        // If found send host var to server
        if ok {
            return true
        }
    }
    
    return false
}

/*
 *  Match string with regex.
 */
func writeHost(pid int32, h *utils.Host) {

    // Form Unix socket based on pid 
    f := utils.RUNPATH + strconv.Itoa(int(pid)) + ".sock"
    log.Println("SOCK: ", f)
    c, err := net.Dial("unix", f)
    utils.Check(err)
    
    defer c.Close()

    // Convert host var to json and send to server
    payload, err := json.Marshal(h)
    utils.Check(err)
    
    // Send to server over unix socket.
    _, err = c.Write(payload)
    utils.Check(err)
}

/*
 *  Get connection information of ssh tunnel and send the 
 *  information to server.
 */
func SendConfig() {
    
    // Get parent proc ID which will be flock's pid.
    ppid := os.Getppid()
    log.Println("ppid = ", ppid)

    // Flock on pid file
    fd := utils.LockFile(ppid)
    
    // Get parent process params
    _, pcmd := getProcParam(int32(ppid))
    log.Println("SSH Process cmdline = ", pcmd)

    //Host to store connection information
    var h utils.Host
    h.Pid = ppid
    
    //Get socket connection parameters in host struct
    getConnParams(int32(ppid), &h)
    
    //Get client config parameters in host struct
    getConfigParams(&h)

    //Log complete host struct.
    log.Println(h)
    
    // Scan through all system processes to find server
    // based on current user id.
    // This is done by regex matching the UID with 
    // commandline of server
    pids, _ := ps.Pids()
    for _, p := range pids  {
        
        // Get proc and commandline based on pid
        _, cmd := getProcParam(p)

        // Check if server 
        ok := match(fmt.Sprintf(`trafficrouter .* -uid %d .*`, os.Getuid()), cmd)
        // If found send host var to server
        if ok {    
            log.Printf("Found Server Process %s, pid = %d\n", cmd, p)
            writeHost(p, &h)
        }            
    }
    
    waitForExit(fd)

}
