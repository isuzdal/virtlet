/*
Copyright 2017 Mirantis

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package imagetranslation

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/golang/glog"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Mirantis/virtlet/pkg/api/virtlet.k8s/v1"
	"github.com/Mirantis/virtlet/pkg/image"
)

type imageNameTranslator struct {
	allowRegexp  bool
	translations map[string]*v1.ImageTranslation
}

// LoadConfigs implements ImageNameTranslator LoadConfigs
func (t *imageNameTranslator) LoadConfigs(ctx context.Context, sources ...ConfigSource) {
	translations := map[string]*v1.ImageTranslation{}
	for _, source := range sources {
		configs, err := source.Configs(ctx)
		if err != nil {
			glog.V(2).Infof("cannot get image translation configs from %s: %v", source.Description(), err)
			continue
		}
		for _, cfg := range configs {
			body, err := cfg.Payload()
			if err != nil {
				glog.V(2).Infof("cannot load image translation config %s from %s: %v", cfg.ConfigName(), source.Description(), err)
				continue
			}

			translations[cfg.ConfigName()] = &body
		}
	}
	t.translations = translations
}

func convertEndpoint(rule v1.TranslationRule, config *v1.ImageTranslation) image.Endpoint {
	profile, exists := config.Transports[rule.Transport]
	if !exists {
		return image.Endpoint{
			URL:          rule.URL,
			MaxRedirects: -1,
		}
	}
	if profile.TimeoutMilliseconds < 0 {
		profile.TimeoutMilliseconds = 0
	}
	maxRedirects := -1
	if profile.MaxRedirects != nil {
		maxRedirects = *profile.MaxRedirects
	}

	var tlsConfig *image.TLSConfig
	if profile.TLS != nil {
		var certificates []image.TLSCertificate
		for i, record := range profile.TLS.Certificates {
			var x509Certs []*x509.Certificate
			var privateKey crypto.PrivateKey

			for _, data := range [2]string{record.Key, record.Cert} {
				dataBytes := []byte(data)
				for {
					block, rest := pem.Decode(dataBytes)
					if block == nil {
						break
					}
					if block.Type == "CERTIFICATE" {
						c, err := x509.ParseCertificate(block.Bytes)
						if err != nil {
							glog.V(2).Infof("error decoding certificate #%d from transport profile %s", i, rule.Transport)
						} else {
							x509Certs = append(x509Certs, c)
						}
					} else if privateKey == nil && strings.HasSuffix(block.Type, "PRIVATE KEY") {
						k, err := parsePrivateKey(block.Bytes)
						if err != nil {
							glog.V(2).Infof("error decoding private key #%d from transport profile %s", i, rule.Transport)
						} else {
							privateKey = k
						}
					}
					dataBytes = rest
				}
			}

			for _, c := range x509Certs {
				certificates = append(certificates, image.TLSCertificate{
					Certificate: c,
					PrivateKey:  privateKey,
				})
			}
		}

		tlsConfig = &image.TLSConfig{
			ServerName:   profile.TLS.ServerName,
			Insecure:     profile.TLS.Insecure,
			Certificates: certificates,
		}
	}

	return image.Endpoint{
		URL:          rule.URL,
		Timeout:      time.Millisecond * time.Duration(profile.TimeoutMilliseconds),
		Proxy:        profile.Proxy,
		ProfileName:  rule.Transport,
		MaxRedirects: maxRedirects,
		TLS:          tlsConfig,
	}
}

func parsePrivateKey(der []byte) (crypto.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		switch key := key.(type) {
		case *rsa.PrivateKey, *ecdsa.PrivateKey:
			return key, nil
		default:
			return nil, errors.New("tls: found unknown private key type in PKCS#8 wrapping")
		}
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}

	return nil, errors.New("tls: failed to parse private key")
}

// Translate implements ImageNameTranslator Translate
func (t *imageNameTranslator) Translate(name string) image.Endpoint {
	for _, translation := range t.translations {
		prefix := translation.Prefix + "/"
		unprefixedName := name
		if prefix != "/" {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			unprefixedName = name[len(prefix):]
		}
		for _, r := range translation.Rules {
			if r.Name != "" && r.Name == unprefixedName {
				return convertEndpoint(r, translation)
			}
		}
		if !t.allowRegexp {
			continue
		}
		for _, r := range translation.Rules {
			if r.Regex == "" {
				continue
			}
			re, err := regexp.Compile(r.Regex)
			if err != nil {
				glog.V(2).Infof("invalid regexp in image translation config: ", r.Regex)
				continue
			}
			submatchIndexes := re.FindStringSubmatchIndex(unprefixedName)
			if len(submatchIndexes) > 0 {
				r.URL = string(re.ExpandString(nil, r.URL, unprefixedName, submatchIndexes))
				return convertEndpoint(r, translation)
			}
		}
	}
	glog.V(1).Infof("Using URL %q without translation", name)
	return image.Endpoint{URL: name, MaxRedirects: -1}
}

// NewImageNameTranslator creates an instance of ImageNameTranslator
func NewImageNameTranslator(allowRegexp bool) ImageNameTranslator {
	return &imageNameTranslator{
		allowRegexp: allowRegexp,
	}
}

// GetDefaultImageTranslator returns a default image translation that
// uses CRDs and a config directory
func GetDefaultImageTranslator(imageTranslationConfigsDir string, allowRegexp bool, clientCfg clientcmd.ClientConfig) image.Translator {
	var sources []ConfigSource
	if clientCfg != nil {
		sources = append(sources, NewCRDSource("kube-system", clientCfg))
	}
	if imageTranslationConfigsDir != "" {
		sources = append(sources, NewFileConfigSource(imageTranslationConfigsDir))
	}
	return func(ctx context.Context, name string) image.Endpoint {
		translator := NewImageNameTranslator(allowRegexp)
		translator.LoadConfigs(ctx, sources...)
		return translator.Translate(name)
	}
}

// GetEmptyImageTranslator returns an empty image translator that
// doesn't apply any translations
func GetEmptyImageTranslator() image.Translator {
	return func(ctx context.Context, name string) image.Endpoint {
		return NewImageNameTranslator(false).Translate(name)
	}
}
