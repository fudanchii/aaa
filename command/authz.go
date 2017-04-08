package command

import (
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/nabeken/aaa/agent"
	"github.com/nabeken/aws-go-s3/bucket"
	"github.com/pkg/errors"
)

type AuthzCommand struct {
	Domain    string `long:"domain" description:"Domain to be authorized" required:"true"`
	Challenge string `long:"challenge" description:"Challenge Type" default:"dns-01"`
}

func (c *AuthzCommand) Execute(args []string) error {
	return (&AuthzService{
		Domain:     c.Domain,
		Challenge:  c.Challenge,
		S3Bucket:   Options.S3Bucket,
		S3KMSKeyID: Options.S3KMSKeyID,
		Email:      Options.Email,
	}).Run()
}

type AuthzService struct {
	Domain     string
	Challenge  string
	S3Bucket   string
	S3KMSKeyID string
	Email      string
}

func (svc *AuthzService) Run() error {
	// initialize S3 bucket and filer
	s3b := bucket.New(s3.New(session.New()), svc.S3Bucket)
	filer := agent.NewS3Filer(s3b, svc.S3KMSKeyID)
	store, err := agent.NewStore(svc.Email, filer)
	if err != nil {
		return errors.Wrap(err, "failed to initialize the store")
	}

	log.Printf("INFO: start authorization for %s with %s", svc.Domain, svc.Challenge)

	newAuthzReq := &agent.NewAuthorizationRequest{
		Identifier: &agent.Identifier{
			Type:  "dns",
			Value: svc.Domain,
		},
	}

	// initialize client here
	client := agent.NewClient(DirectoryURL(), store)
	if err := client.Init(); err != nil {
		return errors.Wrap(err, "failed to initialize the client")
	}

	authzResp, err := client.NewAuthorization(newAuthzReq)
	if err != nil {
		return errors.Wrap(err, "authorization is failed")
	}

	log.Printf("INFO: authorization: %s", authzResp.URL)

	var challenge agent.Challenge
	var challengeSolver agent.ChallengeSolver

	switch svc.Challenge {
	case "dns-01":
		dnsChallenge, found := agent.FindDNSChallenge(authzResp)
		if !found {
			return errors.New("aaa: no DNS challenge and its combination found")
		}

		r53 := agent.NewRoute53Provider(route53.New(session.New()))
		challenge = dnsChallenge
		challengeSolver = agent.NewDNSChallengeSolver(r53, dnsChallenge, svc.Domain)
	default:
		return fmt.Errorf("aaa: challenge %s is not supported")
	}

	publicKey, err := store.LoadPublicKey()
	if err != nil {
		return errors.Wrap(err, "failed to load the public key")
	}

	keyAuthz, err := agent.BuildKeyAuthorization(challenge.Token, publicKey)
	if err != nil {
		return errors.Wrap(err, "failed to build authorizatio key")
	}

	agent.Debug("KeyAuthorization: ", keyAuthz)

	if err := challengeSolver.SolveChallenge(keyAuthz); err != nil {
		return errors.Wrap(err, "failed to solve the challenge")
	}

	if err := client.SolveChallenge(challenge, keyAuthz); err != nil {
		return errors.Wrap(err, "failed to submit the solution")
	}

	if err := client.WaitChallengeDone(challenge); err != nil {
		log.Print("INFO: challenge has been failed")
		return errors.Wrap(err, "failed to do challenge")
	}

	if err := challengeSolver.CleanupChallenge(keyAuthz); err != nil {
		return errors.Wrap(err, "failed to cleanup the challenge")
	}

	// getting the latest authorization status
	currentAuthz, err := client.GetAuthorization(authzResp.URL)
	if err != nil {
		return errors.Wrap(err, "failed to get authorization")
	}

	if err := store.SaveAuthorization(currentAuthz); err != nil {
		return errors.Wrap(err, "failed to save the authorization in the store")
	}

	log.Print("INFO: challenge has been solved")

	return nil
}
