package auth

import (
	"context"

	"github.com/coreos/go-oidc/v3/oidc"

	firebase "firebase.google.com/go"
	firebaseAuth "firebase.google.com/go/auth"
)

type FirebaseClient struct {
    authClient *firebaseAuth.Client
}

func NewFirebaseClient() (*FirebaseClient, error) {
    app, err := firebase.NewApp(context.Background(), nil)
    if err != nil {
        return nil, err
    }
	client, err := app.Auth(context.Background())
	if err != nil {
		return nil, err
	}
    firebaseClient := FirebaseClient {
        authClient: client,
    }
    return &firebaseClient, nil
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
