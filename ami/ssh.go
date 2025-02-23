package ami

import (
	"fmt"
	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	"golang.org/x/crypto/ssh"
)

const (
	Rows     = 80
	Cols     = 160
	TtySpeed = 14400
	Term     = "xterm"
	Network  = "tcp"
)

func OpenNativeSshSession(option *NativeDebugOptions, pipe Pipe) error {
	cfg := &ssh.ClientConfig{
		User: option.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(option.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	server := fmt.Sprintf("%s:%s", option.IP, option.Port)
	conn, err := ssh.Dial(Network, server, cfg)
	if err != nil {
		return errors.Trace(err)
	}
	defer func(conn *ssh.Client) {
		_ = conn.Close()
	}(conn)

	session, err := conn.NewSession()
	if err != nil {
		return errors.Trace(err)
	}
	defer func(session *ssh.Session) {
		_ = session.Close()
	}(session)

	session.Stdout = pipe.OutWriter
	session.Stderr = pipe.OutWriter
	session.Stdin = pipe.InReader

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,        // enable echo
		ssh.TTY_OP_ISPEED: TtySpeed, // input speed = 14.4k baud
		ssh.TTY_OP_OSPEED: TtySpeed, // output speed = 14.4k baud
	}

	// TODO: support window resize
	if err = session.RequestPty(Term, Rows, Cols, modes); err != nil {
		return errors.Trace(err)
	}
	// Start remote shell
	if err = session.Shell(); err != nil {
		return errors.Trace(err)
	}
	err = session.Wait()
	if err != nil {
		// ignore session errors
		log.L().Warn("ssh session log out with exception", log.Error(err))
	}

	return nil
}
