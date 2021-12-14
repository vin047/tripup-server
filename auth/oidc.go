package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

type contextKey string

type OIDCClient struct {
    verifier        *oidc.IDTokenVerifier
    authTokenKey    contextKey
}

// RawOIDCTokenFromHeader tries to retreive the raw OIDC token string from the
// "Authorization" request header, formatted as "Authorization: Bearer TOKEN".
func RawOIDCTokenFromHeader(request *http.Request) (string, error) {
    bearer := request.Header.Get("Authorization")
    if len(bearer) > 7 && strings.ToUpper(bearer[0:6]) == "BEARER" {
        return bearer[7:], nil
    }
    return "", errors.New("unable to extract token from request")
}

func NewOIDCClient(issuer string, clientID string) (*OIDCClient, error) {
    provider, err := oidc.NewProvider(context.Background(), issuer)
    if err != nil {
        return nil, err
    }
    oidcClient := OIDCClient{
        authTokenKey: "auth-token",
        verifier: provider.Verifier(&oidc.Config{
            ClientID: clientID,
        }),
    }
    return &oidcClient, nil
}

// OIDCHandler returns a router middleware for OIDC token verification.
//
// It will check for request authorization by extracting and verifying
// the OIDC token, placing the verified token into the request context
// with the key `authTokenKey`.
func (client *OIDCClient) OIDCHandler() func(next http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        handler := func(response http.ResponseWriter, request *http.Request) {
            idToken, err := client.extractAndVerifyIDToken(request)
            if err != nil {
                response.WriteHeader(http.StatusUnauthorized)
                response.Write([]byte(err.Error()))
                return
            }
            ctx := context.WithValue(request.Context(), client.authTokenKey, idToken)
            next.ServeHTTP(response, request.WithContext(ctx))
        }
        return http.HandlerFunc(handler)
    }
}

// extractAndVerifyIDToken extracts and verifies the OIDC token from the request,
// returning the OIDC token to the caller.
func (client *OIDCClient) extractAndVerifyIDToken(request *http.Request) (*oidc.IDToken, error) {
    rawIDToken, err := RawOIDCTokenFromHeader(request)
    if err != nil {
        return nil, err
    }
    return client.verifier.Verify(request.Context(), rawIDToken)
}

// IDToken gets the OIDC token from the request context using the `authTokenKey`.
func (client *OIDCClient) IDToken(request *http.Request) (*oidc.IDToken, bool) {
    idToken, ok := request.Context().Value(client.authTokenKey).(*oidc.IDToken)
    return idToken, ok
}
