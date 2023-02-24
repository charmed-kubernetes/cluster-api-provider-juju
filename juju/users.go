package juju

import (
	"context"
	"encoding/asn1"
	"encoding/base64"

	"github.com/juju/juju/api/client/usermanager"
	"github.com/juju/juju/jujuclient"
)

type usersClient struct {
	ConnectionFactory
}

func newUsersClient(cf ConnectionFactory) *usersClient {
	return &usersClient{
		ConnectionFactory: cf,
	}
}

func (c *usersClient) ResetPassword(ctx context.Context, controllerName string, controllerEndpoints []string, username string) (string, error) {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return "", err
	}

	client := usermanager.NewClient(conn)
	defer client.Close()

	secretKey, err := client.ResetPassword(username)
	if err != nil {
		return "", err
	}

	return generateUserControllerAccessToken(controllerName, controllerEndpoints, username, secretKey)
}

func generateUserControllerAccessToken(controllerName string, controllerEndpoints []string, username string, secretKey []byte) (string, error) {

	// Generate the base64-encoded string for the user to pass to
	// "juju register". We marshal the information using ASN.1
	// to keep the size down, since we need to encode binary data.
	registrationInfo := jujuclient.RegistrationInfo{
		User:           username,
		Addrs:          controllerEndpoints,
		SecretKey:      secretKey,
		ControllerName: controllerName,
	}

	registrationData, err := asn1.Marshal(registrationInfo)
	if err != nil {
		return "", err
	}

	// Use URLEncoding so we don't get + or / in the string,
	// and pad with zero bytes so we don't get =; this all
	// makes it easier to copy & paste in a terminal.
	//
	// The embedded ASN.1 data is length-encoded, so the
	// padding will not complicate decoding.
	remainder := len(registrationData) % 3
	if remainder != 0 {
		var pad [3]byte
		registrationData = append(registrationData, pad[:3-remainder]...)
	}
	return base64.URLEncoding.EncodeToString(registrationData), nil
}
