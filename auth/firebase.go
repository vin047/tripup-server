package auth

import (
	"context"
	"log"
	"os"

	"github.com/coreos/go-oidc/v3/oidc"
	"google.golang.org/api/option"

	firebase "firebase.google.com/go"
	firebaseAuth "firebase.google.com/go/auth"
)

var client *firebaseAuth.Client
var errLogger = log.New(os.Stderr, "[ERROR] ServerLog: ", log.LstdFlags | log.Lshortfile)

// InitialiseFirebaseAuthBackend initialises the firebase backend client
func InitialiseFirebaseAuthBackend(credentialsFilePath *string) {
	// initialise sdk
	var app *firebase.App
	var err error
	if credentialsFilePath == nil {
		app, err = firebase.NewApp(context.Background(), nil)
	} else {
		opt := option.WithCredentialsFile(*credentialsFilePath)
		app, err = firebase.NewApp(context.Background(), nil, opt)
	}
	if err != nil {
		errLogger.Fatalf("error initializing app: %v\n", err)
	}

	// get auth client
	client, err = app.Auth(context.Background())
	if err != nil {
		errLogger.Fatalf("error getting Auth client: %v\n", err)
	}
}

// AuthProvidersFromToken obtains the authorisation mechanisms from the provided
// token. These fields are provided by Firebase.
func AuthProvidersFromToken(idToken *oidc.IDToken) (*AuthProviders, error) {
	authToken, err := idTokenToFirebaseAuthToken(idToken)
	if err != nil {
		return nil, err
	}
	authProviders := AuthProviders {
		PhoneNumber: shasum256P(identity(authToken, "phone")),
		Email: shasum256P(identity(authToken, "email")),
		AppleID: shasum256P(identity(authToken, "apple.com")),
	}
	return &authProviders, nil
}

// idTokenToFirebaseAuthToken transforms a generic OIDC token into a Firebase Auth token.
func idTokenToFirebaseAuthToken(idToken *oidc.IDToken) (*firebaseAuth.Token, error) {
    var authToken firebaseAuth.Token
    if err := idToken.Claims(&authToken); err != nil {
        return nil, err
    }
	return &authToken, nil
}

// identity obtains the identity, if available, of the given identifier key
// from a Firebase Auth token.
func identity(authToken *firebaseAuth.Token, identifier string) (*string) {
	intf, ok := authToken.Firebase.Identities[identifier].([]interface{})
	if !ok || len(intf) < 1 {
		return nil
	}
	val, ok := intf[0].(string)
	if !ok || len(val) == 0 {
		return nil
	}
	return &val
}
