package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

type socksConn struct {
	// Injected fields
	dialer     *net.Dialer
	username   string
	password   string
	clientConn net.Conn
	logger     Logger
}

func (c *socksConn) closeClientConn(ctxErr error) {
	err := c.clientConn.Close()
	if err != nil && ctxErr == nil {
		c.logger.Warnf("closing client connection: %s", err)
	}
}

func (c *socksConn) run(ctx context.Context) error {
	authMethod := authNotRequired
	if c.username != "" || c.password != "" {
		authMethod = authUsernamePassword
	}

	err := verifyFirstNegotiation(c.clientConn, authMethod)
	if err != nil {
		replyMethod := authMethod
		if errors.Is(err, ErrNoMethodIdentifiers) || errors.Is(err, ErrNoValidMethodIdentifier) {
			replyMethod = authNotAcceptable
		}
		_, writeErr := c.clientConn.Write([]byte{socks5Version, byte(replyMethod)})
		if writeErr != nil {
			c.logger.Warnf("failed writing first negotiation reply: %s", writeErr)
		}
		c.closeClientConn(ctx.Err())
		return fmt.Errorf("verifying first negotiation: %w", err)
	}

	_, err = c.clientConn.Write([]byte{socks5Version, byte(authMethod)})
	if err != nil {
		c.closeClientConn(ctx.Err())
		return fmt.Errorf("writing first negotiation reply: %w", err)
	}

	switch authMethod {
	case authNotRequired, authNotAcceptable:
	case authGssapi:
		panic("not implemented")
		// TODO
	case authUsernamePassword:
		// See https://datatracker.ietf.org/doc/html/rfc1929#section-2
		err = usernamePasswordSubnegotiate(c.clientConn, c.username, c.password)
		if err != nil {
			// If the server returns a `failure' (STATUS value other than X'00') status,
			// it MUST close the connection.
			c.closeClientConn(ctx.Err())
			return fmt.Errorf("subnegotiating username and password: %w", err)
		}
	default:
		panic(fmt.Sprintf("unimplemented auth method %d", authMethod))
	}

	err = c.handleRequest(ctx)
	c.closeClientConn(ctx.Err())
	if err != nil {
		return fmt.Errorf("handling request: %w", err)
	}
	return nil
}

var (
	ErrCommandNotSupported = errors.New("command not supported")
)

func (c *socksConn) handleRequest(ctx context.Context) error {
	const socksVersion = socks5Version
	request, err := decodeRequest(c.clientConn, socksVersion)
	if err != nil {
		c.encodeFailedResponse(c.clientConn, socksVersion, generalServerFailure)
		return err
	}
	if request.command != connect {
		c.encodeFailedResponse(c.clientConn, socksVersion, commandNotSupported)
		return fmt.Errorf("%w: %s", ErrCommandNotSupported, request.command)
	}

	destinationAddress := net.JoinHostPort(request.destination, fmt.Sprint(request.port))
	destinationConn, err := c.dialer.DialContext(ctx, "tcp", destinationAddress)
	if err != nil {
		c.encodeFailedResponse(c.clientConn, socksVersion, generalServerFailure)
		return err
	}
	defer destinationConn.Close()

	destinationServerAddress := destinationConn.LocalAddr().String()
	destinationAddr, destinationPortStr, err := net.SplitHostPort(destinationServerAddress)
	fmt.Println("===", destinationServerAddress)
	if err != nil {
		return err
	}
	destinationPort, err := strconv.Atoi(destinationPortStr)
	if err != nil {
		return err
	}

	var bindAddrType addrType
	if ip := net.ParseIP(destinationAddr); ip != nil {
		if ip.To4() != nil {
			bindAddrType = ipv4
		} else {
			bindAddrType = ipv6
		}
	} else {
		bindAddrType = domainName
	}

	err = c.encodeSuccessResponse(c.clientConn, socksVersion, succeeded, bindAddrType,
		destinationAddr, uint16(destinationPort))
	if err != nil {
		c.encodeFailedResponse(c.clientConn, socksVersion, generalServerFailure)
		return fmt.Errorf("writing successful %s response: %w", request.command, err)
	}

	errc := make(chan error)
	go func() {
		_, err := io.Copy(c.clientConn, destinationConn)
		if err != nil {
			err = fmt.Errorf("from backend to client: %w", err)
		}
		errc <- err
	}()
	go func() {
		_, err := io.Copy(destinationConn, c.clientConn)
		if err != nil {
			err = fmt.Errorf("from client to backend: %w", err)
		}
		errc <- err
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		_ = destinationConn.Close()
		_ = c.clientConn.Close()
		return nil
	}
}

var (
	ErrVersionNotSupported = errors.New("version not supported")
	ErrNoMethodIdentifiers = errors.New("no method identifiers")
)

// See https://datatracker.ietf.org/doc/html/rfc1928#section-3
func verifyFirstNegotiation(reader io.Reader, requiredMethod authMethod) error {
	const headerLength = 2 // version + nMethods bytes
	header := make([]byte, headerLength)
	_, err := io.ReadFull(reader, header[:])
	if err != nil {
		return fmt.Errorf("reading header: %w", err)
	}

	if header[0] != socks5Version {
		return fmt.Errorf("%w: %d", ErrVersionNotSupported, header[0])
	}

	nMethods := header[1]
	if nMethods == 0 {
		return fmt.Errorf("%w", ErrNoMethodIdentifiers)
	}

	methodIdentifiers := make([]byte, nMethods)
	_, err = io.ReadFull(reader, methodIdentifiers)
	if err != nil {
		return fmt.Errorf("reading method identifiers: %w", err)
	}
	for _, methodIdentifier := range methodIdentifiers {
		if methodIdentifier == byte(requiredMethod) {
			return nil
		}
	}

	return makeNoAcceptableMethodError(requiredMethod, methodIdentifiers)
}

var (
	ErrNoValidMethodIdentifier = errors.New("no valid method identifier")
)

func makeNoAcceptableMethodError(requiredAuthMethod authMethod, methodIdentifiers []byte) error {
	methodNames := make([]string, len(methodIdentifiers))
	for i, methodIdentifier := range methodIdentifiers {
		methodNames[i] = fmt.Sprintf("%q", authMethod(methodIdentifier))
	}

	return fmt.Errorf("%w: none of %s matches %s",
		ErrNoValidMethodIdentifier, strings.Join(methodNames, ", "),
		requiredAuthMethod)
}

// See https://datatracker.ietf.org/doc/html/rfc1928#section-4
type request struct {
	command     cmdType
	destination string
	port        uint16
	addressType addrType
}

var (
	ErrRequestSocksVersionMismatch = errors.New("request SOCKS version mismatch")
	ErrAddressTypeNotSupported     = errors.New("address type not supported")
)

func decodeRequest(reader io.Reader, expectedVersion byte) (req request, err error) {
	const headerLength = 4
	header := [headerLength]byte{}
	_, err = io.ReadFull(reader, header[:])
	if err != nil {
		return request{}, fmt.Errorf("reading header: %w", err)
	}

	version := header[0]
	if header[0] != expectedVersion {
		return request{}, fmt.Errorf("%w: expected %d and got %d",
			ErrRequestSocksVersionMismatch, expectedVersion, version)
	}

	req.command = cmdType(header[1])
	// header[2] is RSV byte
	req.addressType = addrType(header[3])

	switch req.addressType {
	case ipv4:
		var ip [4]byte
		_, err = io.ReadFull(reader, ip[:])
		if err != nil {
			return request{}, fmt.Errorf("reading IPv4 address: %w", err)
		}
		req.destination = netip.AddrFrom4(ip).String()
	case ipv6:
		var ip [16]byte
		_, err = io.ReadFull(reader, ip[:])
		if err != nil {
			return request{}, fmt.Errorf("reading IPv6 address: %w", err)
		}
		req.destination = netip.AddrFrom16(ip).String()
	case domainName:
		var header [1]byte
		_, err = io.ReadFull(reader, header[:])
		if err != nil {
			return request{}, fmt.Errorf("reading domain name header: %w", err)
		}
		domainName := make([]byte, header[0])
		_, err = io.ReadFull(reader, domainName)
		if err != nil {
			return request{}, fmt.Errorf("reading domain name bytes: %w", err)
		}
		req.destination = string(domainName)
	default:
		return request{}, fmt.Errorf("%w: %d", ErrAddressTypeNotSupported, req.addressType)
	}

	var portBytes [2]byte
	_, err = io.ReadFull(reader, portBytes[:])
	if err != nil {
		return request{}, fmt.Errorf("reading port: %w", err)
	}
	req.port = binary.BigEndian.Uint16(portBytes[:])

	return req, nil
}
