package round_tripper

import (
	"crypto/x509"

	"code.cloudfoundry.org/gorouter/proxy/error_classifiers"
)

//go:generate counterfeiter -o fakes/fake_retryable_classifier.go . RetryableClassifier
type RetryableClassifier interface {
	IsRetryable(err error) bool
}

type RoundTripperRetryableClassifier struct{}

var retriable = []error_classifiers.Classifier{
	error_classifiers.AttemptedTLSWithNonTLSBackend,
	error_classifiers.Dial,
	error_classifiers.ConnectionResetOnRead,
	error_classifiers.RemoteFailedCertCheck,
	error_classifiers.RemoteHandshakeFailure,
}

func (rc RoundTripperRetryableClassifier) IsRetryable(err error) bool {
	for _, classifier := range retriable {
		if classifier(err) {
			return true
		}
	}

	switch err.(type) {
	case *x509.HostnameError, *x509.UnknownAuthorityError:
		return true
	default:
		return false
	}
}
