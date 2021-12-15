package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/chi"
	"github.com/pressly/chi/middleware"

	"github.com/tripupapp/tripup-server/auth"
	"github.com/tripupapp/tripup-server/database"
	"github.com/tripupapp/tripup-server/notification"
	"github.com/tripupapp/tripup-server/storage"
)

var (
    logger              *log.Logger = log.New(os.Stdout, "[INFO] ServerLog: ", log.LstdFlags)
    errLogger           *log.Logger = log.New(os.Stderr, "[ERROR] ServerLog: ", log.LstdFlags | log.Lshortfile)
    authClient          *auth.OIDCClient
    serverStorageClient storage.StorageClient
    firebaseClient      *auth.FirebaseClient
    notificationService notification.NotificationService
)

type invalidArgError struct {
    argNumber int
}

func (e *invalidArgError) Error() string {
    return fmt.Sprintf("Required argument number %d is an empty string", e.argNumber)
}

func validateArgsNotZero(strings []string) error {
    for index, value := range strings {
        if len(value) == 0 {
            return &invalidArgError{index}
        }
    }
    return nil
}

func getStorageClient(request *http.Request) (storage.StorageClient, error) {
    if serverStorageClient != nil {
        return serverStorageClient, nil
    }
    stringToken, err := auth.RawOIDCTokenFromHeader(request)
    if err != nil {
        return nil, err
    }
    return storage.NewS3Client(stringToken)
}

func main() {
    quit := make(chan os.Signal)                        // set up a channel called 'quit' which takes os signals
    signal.Notify(quit, os.Interrupt, syscall.SIGTERM)  // capture SIGINT from CLI and SIGTERM from OS, redirect to 'quit' channel

    // initialize auth client
    oidcIssuer, exists := os.LookupEnv("OIDC_ISSUER")
    if !exists {
        errLogger.Panicln("OIDC_ISSUER not set")
    }
    oidcClientID, exists := os.LookupEnv("OIDC_CLIENT_ID")
    if !exists {
        errLogger.Panicln("OIDC_CLIENT_ID not set")
    }
    oidcClient, err := auth.NewOIDCClient(oidcIssuer, oidcClientID)
    if err != nil {
        errLogger.Panicln("unable to create OIDC Client:", err.Error())
    }
    authClient = oidcClient

    // initialize optional firebase client
    firClient, err := auth.NewFirebaseClient()
    if err == nil {
        firebaseClient = firClient
    } else {
        logger.Println("firebase admin SDK not in use - ", err.Error())
    }

    // initialize optional storage client
    storageClient, err := storage.NewS3ClientFromEnv()
    if err == nil {
        serverStorageClient = storageClient
    } else {
        logger.Print("using STS for obtaining credentials to the storage server - ", err.Error())
    }

    // initialise notification service
    oneSignalAppID, exists := os.LookupEnv("ONESIGNAL_APPID")
    if !exists {
        errLogger.Panicln("ONESIGNAL_APPID not set")
    }
    oneSignalAPIKey, exists := os.LookupEnv("ONESIGNAL_APIKEY")
    if !exists {
        errLogger.Panicln("ONESIGNAL_APIKEY not set")
    }
    notificationService = notification.OneSignal{AppID: oneSignalAppID, APIKey: oneSignalAPIKey}

    // initialise neo4j database connection
    neoDB := database.Instance()
    neoDB.Connect()

    // initialise the router
    router := chi.NewRouter()
    timeout, err := time.ParseDuration(os.Getenv("TRIPUP_SERVER_TIMEOUT"))
    if err != nil {
        errLogger.Panicln(err)
    }
    throttle, err := strconv.Atoi(os.Getenv("TRIPUP_SERVER_MAX_REQ"))
    if err != nil {
        errLogger.Panicln(err)
    }

    router.Use(authClient.OIDCHandler())    // OIDC authorization middleware
    router.Use(middleware.Timeout(timeout)) // stop processing request after X seconds

    // setup routing
    router.Get("/ping", apiPing)

    router.Route("/users", func(subrouter chi.Router) {
        subrouter.Post("/", apiCreateUser)
        subrouter.Post("/public", apiGetUsersFromAddressable)
        subrouter.Get("/self", apiGetUUID)
        subrouter.Put("/self/contact", apiUpdateUserContact)
        subrouter.Get("/{userID}", apiGetUser)
        subrouter.Put("/self/claims", apiSetSelfHostS3Permission)
    })
    router.Route("/assets", func(subrouter chi.Router) {
        subrouter.Use(middleware.Throttle(throttle))    // max 10 requests processed at same time, backlog others
        subrouter.Get("/", apiGetAssets)
        subrouter.Post("/", apiCreateAsset)
        subrouter.Patch("/", apiPatchAssets)
        subrouter.Patch("/original", apiPatchAssetsRemoteOriginalPaths)
        subrouter.Patch("/originalfilenames", apiPatchAssetsOriginalFilenames)
        subrouter.Put("/{assetID}/original", apiUpdateOriginalRemote)
        subrouter.Put("/{assetID}/originalfilename", apiPutAssetOriginalFilename)
    })
    router.Route("/groups", func(subrouter chi.Router) {
        subrouter.Use(middleware.Throttle(throttle))    // max 10 requests processed at same time, backlog others
        subrouter.Get("/", apiGetGroups)
        subrouter.Post("/", apiCreateGroup)
        subrouter.Get("/album", apiGetAssetsForAllGroups)
        subrouter.Put("/{groupID}", apiJoinGroup)                               // join group by replacing groupkey and linking shared assets
        subrouter.Delete("/{groupID}", apiLeaveGroup)
        subrouter.Get("/{groupID}/users", apiGetGroupUsers)
        subrouter.Patch("/{groupID}/users", apiAddUsersToGroup)                 // add and remove users
        subrouter.Patch("/{groupID}/album", apiAmendGroupAssets)                // add and remove assets
        subrouter.Patch("/{groupID}/album/shared", apiAmendGroupSharedAssets)   // share and unshare assets
    })

    router.Route("/info", func(subrouter chi.Router) {
        throttle, err := strconv.Atoi(os.Getenv("TRIPUP_SERVER_MAX_REQ"))
        if err != nil {
            errLogger.Panicln(err)
        }
        subrouter.Use(middleware.Throttle(throttle))    // max 10 requests processed at same time, backlog others
        subrouter.Post("/validids", APIValidateIDs)             // POST  /info/validids
    })

    router.Route("/schema", func(subrouter chi.Router) {
        subrouter.Use(middleware.Throttle(throttle))    // max 10 requests processed at same time, backlog others
        subrouter.Route("/0", func(subrouter chi.Router) {
            subrouter.Get("/", apiGetSchema0)
            subrouter.Patch("/", apiPatchSchema0)
        })
    })

    // init server, assign 'router' as the handler
    apiServer := &http.Server{ Addr: ":" + os.Getenv("TRIPUP_SERVER_PORT"), Handler: router }

    go func() {
        <-quit      // block and wait for incoming data (SIGINT) on 'quit' channel
        logger.Println("server shutdown command received")
        apiServer.Shutdown(context.Background())
    }()

    logger.Println("server initialised successfully, listening on port", os.Getenv("TRIPUP_SERVER_PORT"))
    // start server, main thread will pause here
    if err := apiServer.ListenAndServe(); err != http.ErrServerClosed {
        errLogger.Println(err)
    }

    logger.Println("server shutdown complete")
}

func apiPing(response http.ResponseWriter, request *http.Request) {
    ping(response, request, database.Instance())
}

func apiGetUUID(response http.ResponseWriter, request *http.Request) {
    getUUID(response, request, database.Instance())
}

func apiCreateUser(response http.ResponseWriter, request *http.Request) {
    createUser(response, request, database.Instance())
}

func apiUpdateUserContact(response http.ResponseWriter, request *http.Request) {
    updateUserContact(response, request, database.Instance())
}

func apiGetUser(response http.ResponseWriter, request *http.Request) {
    getUser(response, request, database.Instance())
}

func apiCreateGroup(response http.ResponseWriter, request *http.Request) {
    createGroup(response, request, database.Instance())
}

func apiGetGroups(response http.ResponseWriter, request *http.Request) {
    getGroups(response, request, database.Instance())
}

func apiJoinGroup(response http.ResponseWriter, request *http.Request) {
    joinGroup(response, request, database.Instance())
}

func apiAddUsersToGroup(response http.ResponseWriter, request *http.Request) {
    addUsersToGroup(response, request, database.Instance())
}

func APIValidateIDs(response http.ResponseWriter, request *http.Request) {
    ValidateIDs(response, request, database.Instance())
}

func apiGetUsersFromAddressable(response http.ResponseWriter, request *http.Request) {
    getUsersFromAddressable(response, request, database.Instance())
}

func apiGetGroupUsers(response http.ResponseWriter, request *http.Request) {
    getGroupUsers(response, request, database.Instance())
}

func apiCreateAsset(response http.ResponseWriter, request *http.Request) {
    createAsset(response, request, database.Instance())
}

func apiPatchAssets(response http.ResponseWriter, request *http.Request) {
    patchAssets(response, request, database.Instance())
}

func apiPatchAssetsRemoteOriginalPaths(response http.ResponseWriter, request *http.Request) {
    patchAssetsRemoteOriginalPaths(response, request, database.Instance())
}

func apiUpdateOriginalRemote(response http.ResponseWriter, request *http.Request) {
    putAssetRemotePathOriginal(response, request, database.Instance())
}

func apiPutAssetOriginalFilename(response http.ResponseWriter, request *http.Request) {
    putAssetOriginalFilename(response, request, database.Instance())
}

func apiPatchAssetsOriginalFilenames(response http.ResponseWriter, request *http.Request) {
    patchAssetsOriginalFilenames(response, request, database.Instance())
}

func apiGetAssets(response http.ResponseWriter, request *http.Request) {
    getAssets(response, request, database.Instance())
}

func apiGetSchema0(response http.ResponseWriter, request *http.Request) {
    getAssetsSchema0(response, request, database.Instance())
}

func apiPatchSchema0(response http.ResponseWriter, request *http.Request) {
    patchSchema0(response, request, database.Instance())
}

func apiGetAssetsForAllGroups(response http.ResponseWriter, request *http.Request) {
    getAssetsForAllGroups(response, request, database.Instance())
}

func apiAmendGroupSharedAssets(response http.ResponseWriter, request *http.Request) {
    amendGroupSharedAssets(response, request, database.Instance())
}

func APISetFavourite(response http.ResponseWriter, request *http.Request) {
    SetFavourite(response, request, database.Instance())
}

func apiLeaveGroup(response http.ResponseWriter, request *http.Request) {
    leaveGroup(response, request, database.Instance())
}

func apiAmendGroupAssets(response http.ResponseWriter, request *http.Request) {
    amendGroupAssets(response, request, database.Instance())
}

func apiSetSelfHostS3Permission(response http.ResponseWriter, request *http.Request) {
    setSelfHostS3Permission(response, request, database.Instance())
}

func GenericErrorHandler(response http.ResponseWriter) {
    if recovery := recover(); recovery != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(recovery)
    }
}

func ping(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    response.WriteHeader(http.StatusOK)
    response.Write([]byte("TripUp"))
}

func getUUID(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    data, err := neoDB.GetUser(token.Subject)

    switch err {
    case nil:
        dataJSON, err := json.Marshal(data)
        if err != nil {
            response.WriteHeader(http.StatusInternalServerError)
            errLogger.Println(err.Error())
        } else {
            response.WriteHeader(http.StatusOK)
            response.Write(dataJSON)
        }
    case io.EOF:
        response.WriteHeader(http.StatusNoContent)
    default:
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    }
}

func createUser(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    type User struct {
        Publickey           string
        Privatekey          string
    }
    var user User
    if err := json.NewDecoder(request.Body).Decode(&user); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    if err := validateArgsNotZero([]string{user.Publickey, user.Privatekey}); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte(err.Error()))
        return
    }

    authProviders, err := auth.AuthProvidersFromToken(token)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Printf("Invalid auth providers – %+v\n", authProviders)
        return
    }

    userid := uuid.New()
    // TODO: check user id not in use

    err = neoDB.CreateUser(token.Subject, userid.String(), authProviders, user.Publickey, user.Privatekey, "1")
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusCreated)
        response.Write([]byte(userid.String()))
    }
}

func updateUserContact(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    authProviders, err := auth.AuthProvidersFromToken(token)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Printf("Invalid auth providers – %+v\n", authProviders)
        return
    }

    err = neoDB.UpdateUserContact(token.Subject, authProviders)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusOK)
    }
}

func getUser(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    userID := chi.URLParam(request, "userID")
    if _, err := uuid.Parse(userID); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Invalid UUID string for User ID"))
        return
    }

    existingMatches, _, err := neoDB.GetPublicInfoForUsers([]string{userID}, []string{}, []string{})
    switch err {
    case nil:
        var publicKey = existingMatches[userID]
        response.WriteHeader(http.StatusOK)
        response.Write([]byte(publicKey))
    case io.EOF:
        response.WriteHeader(http.StatusNoContent)
    default:
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    }
}

func getGroups(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    data, err := neoDB.GetGroups(token.Subject)
    switch err {
    case nil:
        dataJSON, err := json.Marshal(data)
        if err != nil {
            response.WriteHeader(http.StatusInternalServerError)
            errLogger.Printf("Unable to marshal JSON. Error is:\n%s\n", err.Error())
            return
        }
        response.WriteHeader(http.StatusOK)
        response.Write(dataJSON)
    case io.EOF:
        response.WriteHeader(http.StatusNoContent)
    default:
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    }
}

func joinGroup(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    groupID := chi.URLParam(request, "groupID")
    if _, err := uuid.Parse(groupID); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Invalid UUID string for Group ID"))
        return
    }

    var group struct {
        Key    string
    }
    if err := json.NewDecoder(request.Body).Decode(&group); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    err := neoDB.JoinGroup(token.Subject, groupID, group.Key)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusCreated)

        // notify users
        var userIDs []string
        groupUsers, err := neoDB.GetUsersInGroup(token.Subject, groupID)
        if err == io.EOF {
            return
        }
        for userID := range groupUsers {
            userIDs = append(userIDs, userID)
        }
        err = notificationService.Notify(userIDs, notification.UserJoinedGroup, &map[string]string{"groupid": groupID})
        if err != nil {
            errLogger.Println(err.Error())
            return
        }
    }
}

func createGroup(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    var group struct {
        Name    string
        Key     string
    }
    if err := json.NewDecoder(request.Body).Decode(&group); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    if err := validateArgsNotZero([]string{group.Name, group.Key}); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte(err.Error()))
        return
    }

    groupid := uuid.New()
    // TODO: verify trip uuid isn't already in use

    err := neoDB.CreateGroup(token.Subject, groupid.String(), group.Name, group.Key)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusCreated)
        response.Write([]byte(groupid.String()))
    }
}

func addUsersToGroup(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    groupID := chi.URLParam(request, "groupID")
    if _, err := uuid.Parse(groupID); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Invalid UUID string for Group ID"))
        return
    }

    var payload struct {
        Users []map[string]string
    }
    if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    if len(payload.Users) == 0 {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Empty data supplied"))
        return
    }

    err := neoDB.AddUsersToGroup(token.Subject, groupID, payload.Users)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusOK)

        // notify users
        var userIDs []string
        for _, user := range payload.Users {
            userIDs = append(userIDs, user["uuid"])
        }
        err = notificationService.Notify(userIDs, notification.GroupInvite, nil)
        if err != nil {
            errLogger.Println(err.Error())
            return
        }
    }
}

func ValidateIDs(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    type RequestData struct {
        ArrayOfIDs []string
    }

    var ids RequestData
    if err := json.NewDecoder(request.Body).Decode(&ids); err != nil {
        errLogger.Panicln(err)
    }

    result, err := neoDB.VerifyUUIDS(ids.ArrayOfIDs)
    if err == io.EOF {
        logger.Println("no valid ids found")
        response.WriteHeader(http.StatusNoContent)
        return
    }

    dataJson, err := json.Marshal(result)
    if err != nil {
        errLogger.Panicln(err)
    }
    response.WriteHeader(http.StatusOK)
    response.Write(dataJson)
}

func getUsersFromAddressable(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    var contacts struct {
        Uuids   []string
        Numbers []string
        Emails  []string
    }
    if err := json.NewDecoder(request.Body).Decode(&contacts); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    if len(contacts.Uuids) == 0 && len(contacts.Numbers) == 0 && len(contacts.Emails) == 0 {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("No addresses provided"))
    }

    existingMatches, newMatches, err := neoDB.GetPublicInfoForUsers(contacts.Uuids, contacts.Numbers, contacts.Emails)
    switch err {
    case nil:
        result := map[string]interface{} {
            "uuids": existingMatches,
            "otherIdentifiers": newMatches,
        }
        dataJSON, err := json.Marshal(result)
        if err != nil {
            response.WriteHeader(http.StatusInternalServerError)
            errLogger.Println(err.Error())
            return
        }
        response.WriteHeader(http.StatusOK)
        response.Write(dataJSON)
    case io.EOF:
        response.WriteHeader(http.StatusNoContent)
    default:
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    }
}

func getGroupUsers(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    groupID := chi.URLParam(request, "groupID")
    if _, err := uuid.Parse(groupID); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Invalid UUID string for Group ID"))
        return
    }

    data, err := neoDB.GetUsersInGroup(token.Subject, groupID)
    if err == io.EOF {
        response.WriteHeader(http.StatusNoContent)
        return
    }

    dataJSON, err := json.Marshal(data)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        response.Write([]byte("Unable to marshal JSON"))
        return
    }
    response.WriteHeader(http.StatusOK)
    response.Write(dataJSON)
}

type asset struct {
    AssetID string
    Type string
    RemotePath string
    RemotePathOrig *string
    CreateDate *string
    Location *string
    Duration *string
    OriginalFilename *string
    OriginalUTI *string
    PixelWidth int
    PixelHeight int
    Md5 string
    Key string
}

func createAsset(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    var asset asset
    if err := json.NewDecoder(request.Body).Decode(&asset); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    storageClient, err := getStorageClient(request)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
        return
    }

    httpStatus, err, totalsize := createSingleAsset(asset, token.Subject, storageClient, neoDB)
    if err != nil {
        response.WriteHeader(httpStatus)
        if httpStatus == http.StatusInternalServerError {
            errLogger.Println(err.Error())
        } else {
            response.Write([]byte(err.Error()))
        }
        return
    }

    response.WriteHeader(http.StatusCreated)
    if totalsize != nil {
        b := make([]byte, 8)
        binary.LittleEndian.PutUint64(b, *totalsize)
        response.Write(b)
    }
}

func patchAssets(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    var payload struct {
        CREATE []asset  `json:",omitempty"`
        DELETE []string `json:",omitempty"`
    }
    if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    storageClient, err := getStorageClient(request)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
        return
    }

    var httpStatus int
    var resultData = make(map[string]int)

    if len(payload.CREATE) != 0 {
        for _, asset := range payload.CREATE {
            var totalsize *uint64
            httpStatus, err, totalsize = createSingleAsset(asset, token.Subject, storageClient, neoDB)
            if err != nil {
                break
            }
            if totalsize != nil {
                resultData[asset.AssetID] = int(*totalsize)
            }
        }
    }

    if err != nil {
        response.WriteHeader(httpStatus)
        if httpStatus == http.StatusInternalServerError {
            errLogger.Println(err.Error())
        } else {
            response.Write([]byte(err.Error()))
        }
        return
    }

    if len(payload.DELETE) != 0 {
        httpStatus, err = deleteAssets(payload.DELETE, token.Subject, storageClient, neoDB)
    }

    if err != nil {
        response.WriteHeader(httpStatus)
        if httpStatus == http.StatusInternalServerError {
            errLogger.Println(err.Error())
        } else {
            response.Write([]byte(err.Error()))
        }
        return
    }

    if len(resultData) == 0 {
        response.WriteHeader(http.StatusOK)
    } else {
        dataJSON, err := json.Marshal(resultData)
        if err != nil {
            response.WriteHeader(http.StatusInternalServerError)
            errLogger.Println(err.Error())
        } else {
            response.WriteHeader(http.StatusOK)
            response.Write(dataJSON)
        }
    }
}

func createSingleAsset(asset asset, uid string, storageClient storage.StorageClient, neoDB *database.Neo4j) (int, error, *uint64) {
    if err := validateArgsNotZero([]string{asset.AssetID, asset.RemotePath, asset.Key}); err != nil {
        return http.StatusBadRequest, err, nil
    }

    if asset.PixelWidth == 0 || asset.PixelHeight == 0 {
        return http.StatusBadRequest, errors.New("One of the Int args has a value of 0"), nil
    }

    var totalsize *uint64
    if asset.RemotePathOrig != nil {
        originalLength, lowLength, err := storageClient.Filesizes(*asset.RemotePathOrig)
        // 128 KB minimum
        if originalLength < 131072 {
            originalLength = 131072
        }
        if lowLength < 131072 {
            lowLength = 131072
        }
        if err != nil {
            errLogger.Println(*asset.RemotePathOrig)
            return http.StatusInternalServerError, err, nil
        }
        size := originalLength + lowLength
        totalsize = &size
    }

    if err := validateArgsNotZero([]string{asset.Type}); err != nil {
        asset.Type = "photo"
    }

    err := neoDB.CreateAsset(uid, asset.AssetID, asset.Type, asset.RemotePath, asset.CreateDate, asset.Location, asset.Duration, asset.OriginalFilename, asset.OriginalUTI, asset.PixelWidth, asset.PixelHeight, asset.Md5, asset.Key, asset.RemotePathOrig, totalsize)
    if err != nil {
        return http.StatusInternalServerError, err, nil
    }
    return http.StatusCreated, nil, totalsize
}

func deleteAssets(assetIDs []string, uid string, storageClient storage.StorageClient, neoDB *database.Neo4j) (int, error) {
    if len(assetIDs) == 0 {
        return http.StatusBadRequest, errors.New("AssetIDs is empty")
    }

    objectsToDelete, err := neoDB.DeleteAssets(uid, assetIDs)
    if err != nil {
        return http.StatusInternalServerError, err
    }

    err = storageClient.Delete(*objectsToDelete)
    if err != nil {
        return http.StatusInternalServerError, err
    }

    return http.StatusOK, nil
}

func patchAssetsRemoteOriginalPaths(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    var payload map[string]string
    if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
        errLogger.Panicln(err)
    }

    if len(payload) == 0 {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("payload is empty"))
        return
    }

    storageClient, err := getStorageClient(request)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
        return
    }

    var resultData = make(map[string]int)
    for assetID, remotePathOriginal := range payload {
        originalLength, lowLength, err := storageClient.Filesizes(remotePathOriginal)
        // 128 KB minimum
        if originalLength < 131072 {
            originalLength = 131072
        }
        if lowLength < 131072 {
            lowLength = 131072
        }
        if err != nil {
            break
        }

        err = neoDB.AddPathForOriginalAsset(token.Subject, assetID, remotePathOriginal, originalLength + lowLength)
        if err != nil {
            break
        }

        resultData[assetID] = int(originalLength) + int(lowLength)
    }

    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
        return
    }

    dataJSON, err := json.Marshal(resultData)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusOK)
        response.Write(dataJSON)
    }
}

func putAssetRemotePathOriginal(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    assetID := chi.URLParam(request, "assetID")
    if _, err := uuid.Parse(assetID); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Invalid UUID string for Asset ID"))
        return
    }

    type assetUpdate struct {
        Remotepathorig string
    }

    var asset assetUpdate
    if err := json.NewDecoder(request.Body).Decode(&asset); err != nil {
        errLogger.Panicln(err)
    }

    if err := validateArgsNotZero([]string{asset.Remotepathorig}); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte(err.Error()))
        return
    }

    storageClient, err := getStorageClient(request)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
        return
    }

    originalLength, lowLength, err := storageClient.Filesizes(asset.Remotepathorig)
    // 128 KB minimum
    if originalLength < 131072 {
        originalLength = 131072
    }
    if lowLength < 131072 {
        lowLength = 131072
    }
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    }

    err = neoDB.AddPathForOriginalAsset(token.Subject, assetID, asset.Remotepathorig, originalLength + lowLength)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
        return
    }

    response.WriteHeader(http.StatusOK)
}

func putAssetOriginalFilename(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    assetID := chi.URLParam(request, "assetID")
    if _, err := uuid.Parse(assetID); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Invalid UUID string for Asset ID"))
        return
    }

    var payload struct {
        Originalfilename    string
    }
    if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte(err.Error()))
        return
    }

    var data = map[string]string {
        assetID: payload.Originalfilename,
    }
    if err := neoDB.SetAssetsOriginalFilenames(token.Subject, data); err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusOK)
    }
}

func patchAssetsOriginalFilenames(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    var payload map[string]string
    if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte(err.Error()))
        return
    }

    if len(payload) == 0 {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("payload is empty"))
        return
    }

    if err := neoDB.SetAssetsOriginalFilenames(token.Subject, payload); err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusOK)
    }
}

func amendGroupSharedAssets(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    groupID := chi.URLParam(request, "groupID")
    if _, err := uuid.Parse(groupID); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Invalid UUID string for Group ID"))
        return
    }

    var requestData struct {
        AssetKeys []string  `json:",omitempty"`
        AssetIDs []string
        Share bool
    }
    if err := json.NewDecoder(request.Body).Decode(&requestData); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    if len(requestData.AssetIDs) == 0 {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("No asset ids provided for request"))
        return
    }

    if requestData.Share && (len(requestData.AssetKeys) == 0 || (len(requestData.AssetIDs) != len(requestData.AssetKeys))) {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("No asset keys provided for request"))
        return
    }

    var err error
    if requestData.Share {
        err = neoDB.ShareAssets(token.Subject, groupID, requestData.AssetIDs, requestData.AssetKeys)
    } else {
        err = neoDB.UnshareAssets(token.Subject, groupID, requestData.AssetIDs)
    }

    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusOK)

        // notify users
        var userIDs []string
        groupUsers, err := neoDB.GetUsersInGroup(token.Subject, groupID)
        if err == io.EOF {
            return
        }
        for userID := range groupUsers {
            userIDs = append(userIDs, userID)
        }
        if requestData.Share {
            err = notificationService.Notify(userIDs, notification.AssetsAddedToGroupByUser, &map[string]string{"groupid": groupID})
        } else {
            err = notificationService.Notify(userIDs, notification.AssetsChangedForGroup, &map[string]string{"groupid": groupID})
        }
        if err != nil {
            errLogger.Println(err.Error())
            return
        }
    }
}

func SetFavourite(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    type Props struct {
        TripID string
        ImageID string
        Favourite bool
    }

    // parse request body for photo details
    var props Props
    if err := json.NewDecoder(request.Body).Decode(&props); err != nil {
        errLogger.Panicln(err)
    }

    if props.Favourite {
        neoDB.SetFavourite(token.Subject, props.TripID, props.ImageID)
    } else {
        neoDB.UnsetFavourite(token.Subject, props.TripID, props.ImageID)
    }

    response.WriteHeader(http.StatusOK)
}

func patchSchema0(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    var patchData struct {
        AssetKeys map[string]string   `json:",omitempty"`
        AssetMD5s map[string]string   `json:",omitempty"`
    }
    if err := json.NewDecoder(request.Body).Decode(&patchData); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    if err := neoDB.PatchSchema0(token.Subject, patchData.AssetKeys, patchData.AssetMD5s); err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
        return
    }
    response.WriteHeader(http.StatusOK)
}

func getAssets(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    data, err := neoDB.GetAssets(token.Subject)
    switch err {
    case nil:
        dataJSON, err := json.Marshal(data)
        if err != nil {
            response.WriteHeader(http.StatusInternalServerError)
            errLogger.Println(err.Error())
        } else {
            response.WriteHeader(http.StatusOK)
            response.Write(dataJSON)
        }
    case io.EOF:
        response.WriteHeader(http.StatusNoContent)
    default:
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    }
}

func getAssetsSchema0(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    data, err := neoDB.GetAssetsSchema0(token.Subject)
    switch err {
    case nil:
        dataJSON, err := json.Marshal(data)
        if err != nil {
            response.WriteHeader(http.StatusInternalServerError)
            errLogger.Println(err.Error())
        } else {
            response.WriteHeader(http.StatusOK)
            response.Write(dataJSON)
        }
    case io.EOF:
        response.WriteHeader(http.StatusNoContent)
    default:
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    }
}

func getAssetsForAllGroups(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    data, err := neoDB.GetAssetsForAllGroups(token.Subject)

    switch err {
    case nil:
        dataJSON, err := json.Marshal(data)
        if err != nil {
            response.WriteHeader(http.StatusInternalServerError)
            errLogger.Println(err.Error())
        } else {
            response.WriteHeader(http.StatusOK)
            response.Write(dataJSON)
        }
    case io.EOF:
        response.WriteHeader(http.StatusNoContent)
    default:
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    }
}

func leaveGroup(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    groupID := chi.URLParam(request, "groupID")
    if _, err := uuid.Parse(groupID); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Invalid UUID string for Group ID"))
        return
    }

    err := neoDB.LeaveGroup(token.Subject, groupID)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusOK)

        // notify users
        var userIDs []string
        groupUsers, err := neoDB.GetUsersInGroup(token.Subject, groupID)
        if err == io.EOF {
            return
        }
        for userID := range groupUsers {
            userIDs = append(userIDs, userID)
        }
        err = notificationService.Notify(userIDs, notification.UserLeftGroup, &map[string]string{"groupid": groupID})
        if err != nil {
            errLogger.Println(err.Error())
            return
        }
    }
}

func amendGroupAssets(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    groupID := chi.URLParam(request, "groupID")
    if _, err := uuid.Parse(groupID); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Invalid UUID string for Group ID"))
        return
    }

    var requestData struct {
        Add         bool
        AssetIDs    []string
    }
    if err := json.NewDecoder(request.Body).Decode(&requestData); err != nil {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("Unable to decode JSON payload"))
        return
    }

    if len(requestData.AssetIDs) == 0 {
        response.WriteHeader(http.StatusBadRequest)
        response.Write([]byte("No asset IDs provided for request"))
        return
    }

    var err error
    if requestData.Add {
        err = neoDB.AddAssetsToGroup(token.Subject, groupID, requestData.AssetIDs)
    } else {
        err = neoDB.RemoveAssetsFromGroup(token.Subject, groupID, requestData.AssetIDs)
    }

    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
    } else {
        response.WriteHeader(http.StatusOK)

        if !requestData.Add {
            // notify users
            var userIDs []string
            groupUsers, err := neoDB.GetUsersInGroup(token.Subject, groupID)
            if err == io.EOF {
                return
            }
            for userID := range groupUsers {
                userIDs = append(userIDs, userID)
            }
            err = notificationService.Notify(userIDs, notification.AssetsChangedForGroup, &map[string]string{"groupid": groupID})
            if err != nil {
                errLogger.Println(err.Error())
                return
            }
        }
    }
}

func setSelfHostS3Permission(response http.ResponseWriter, request *http.Request, neoDB *database.Neo4j) {
    defer GenericErrorHandler(response)

    if firebaseClient == nil {
        response.WriteHeader(http.StatusNotImplemented)
        return
    }

    token, ok := authClient.IDToken(request)
    if !ok {
        response.WriteHeader(http.StatusInternalServerError)
        return
    }

    err := firebaseClient.SetSelfHostS3Claim(request.Context(), token.Subject)
    if err != nil {
        response.WriteHeader(http.StatusInternalServerError)
        errLogger.Println(err.Error())
        return
    }
    response.WriteHeader(http.StatusOK)
}
