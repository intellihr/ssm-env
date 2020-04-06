package main

import (
  "bytes"
  "flag"
  "fmt"
  "github.com/aws/aws-sdk-go/aws/awserr"
  "net"
  "net/http"
  "os"
  "os/exec"
  "reflect"
  "strconv"
  "strings"
  "syscall"
  "text/template"
  "time"

  "github.com/aws/aws-sdk-go/aws"
  "github.com/aws/aws-sdk-go/aws/ec2metadata"
  "github.com/aws/aws-sdk-go/aws/session"
  "github.com/aws/aws-sdk-go/service/ssm"

   "golang.org/x/net/http2"

)

const (
  // DefaultTemplate is the default template used to determine what the SSM
  // parameter name is for an environment variable.
  DefaultTemplate = `{{ if hasPrefix .Value "ssm://" }}{{ trimPrefix .Value "ssm://" }}{{ end }}`

  // defaultBatchSize is the default number of parameters to fetch at once.
  // The SSM API limits this to a maximum of 10 at the time of writing.
  defaultBatchSize = 10
)

// TemplateFuncs are helper functions provided to the template.
var TemplateFuncs = template.FuncMap{
  "contains":   strings.Contains,
  "hasPrefix":  strings.HasPrefix,
  "hasSuffix":  strings.HasSuffix,
  "trimPrefix": strings.TrimPrefix,
  "trimSuffix": strings.TrimSuffix,
  "trimSpace":  strings.TrimSpace,
  "trimLeft":   strings.TrimLeft,
  "trimRight":  strings.TrimRight,
  "trim":       strings.Trim,
  "title":      strings.Title,
  "toTitle":    strings.ToTitle,
  "toLower":    strings.ToLower,
  "toUpper":    strings.ToUpper,
}

func main() {
  var (
    template = flag.String("template", DefaultTemplate, "The template used to determine what the SSM parameter name is for an environment variable. When this template returns an empty string, the env variable is not an SSM parameter")
    decrypt  = flag.Bool("with-decryption", false, "Will attempt to decrypt the parameter, and set the env var as plaintext")
    nofail  = flag.Bool("no-fail", false, "Don't fail if error retrieving parameter")
    credretry = flag.Bool("credential-retry", false, "Will retry indefinitely if NoCredentialProviders occurs")
  )
  flag.Parse()
  args := flag.Args()

  if len(args) <= 0 {
    flag.Usage()
    os.Exit(1)
  }

  path, err := exec.LookPath(args[0])
  must(err)

  var osEnv osEnviron

  t, err := parseTemplate(*template)
  must(err)

  e := &expander{
    batchSize: defaultBatchSize,
    t:         t,
    ssm:       ssm.New(session.Must(awsSession())),
    os:        osEnv,
  }
  must(e.expandEnviron(*decrypt, *nofail, *credretry))
  must(syscall.Exec(path, args[0:], osEnv.Environ()))
}

func awsSession() (*session.Session, error) {
  sess := session.Must(session.NewSession(&aws.Config{
    HTTPClient: NewHTTPClient(),
  }))
  if len(aws.StringValue(sess.Config.Region)) == 0 {
    meta := ec2metadata.New(sess)
    identity, err := meta.GetInstanceIdentityDocument()
    if err != nil {
      awsErr := err.(awserr.Error)
      if awsErr.Code() == "EC2MetadataRequestError" {
        return sess, nil
      }
      return nil, err
    }
    return session.NewSession(&aws.Config{
      Region: aws.String(identity.Region),
      HTTPClient: NewHTTPClient(),
    })
  }
  return sess, nil
}

func parseTemplate(templateText string) (*template.Template, error) {
  return template.New("template").Funcs(TemplateFuncs).Parse(templateText)
}

type ssmClient interface {
  GetParameters(*ssm.GetParametersInput) (*ssm.GetParametersOutput, error)
}

type environ interface {
  Environ() []string
  Setenv(key, vale string)
}

type osEnviron int

func (e osEnviron) Environ() []string {
  return os.Environ()
}

func (e osEnviron) Setenv(key, val string) {
  os.Setenv(key, val)
}

type ssmVar struct {
  envvar    string
  parameter string
}

type expander struct {
  t         *template.Template
  ssm       ssmClient
  os        environ
  batchSize int
}

func (e *expander) parameter(k, v string) (*string, error) {
  b := new(bytes.Buffer)
  if err := e.t.Execute(b, struct{ Name, Value string }{k, v}); err != nil {
    return nil, err
  }

  if p := b.String(); p != "" {
    return &p, nil
  }

  return nil, nil
}

func (e *expander) expandEnviron(decrypt bool, nofail bool, credretry bool) error {
  // Environment variables that point to some SSM parameters.
  var ssmVars []ssmVar

  uniqNames := make(map[string]bool)
  for _, envvar := range e.os.Environ() {
    k, v := splitVar(envvar)

    parameter, err := e.parameter(k, v)
    if err != nil {
      return fmt.Errorf("determining name of parameter: %v", err)
    }

    if parameter != nil {
      uniqNames[*parameter] = true
      ssmVars = append(ssmVars, ssmVar{k, *parameter})
    }
  }

  if len(uniqNames) == 0 {
    // Nothing to do, no SSM parameters.
    return nil
  }

  names := make([]string, len(uniqNames))
  i := 0
  for k := range uniqNames {
    names[i] = k
    i++
  }

  for i := 0; i < len(names); i += e.batchSize {
    j := i + e.batchSize
    if j > len(names) {
      j = len(names)
    }

    var values map[string]string
    for {
      tmpValues, err := e.getParameters(names[i:j], decrypt, nofail)
      values = tmpValues
      if err != nil {
        if reflect.TypeOf(err).Kind() != reflect.TypeOf(new(invalidParametersError)).Kind() {
          awsErr := err.(awserr.Error)
          if awsErr.Code() == "NoCredentialProviders" && credretry {
            fmt.Fprintf(os.Stderr, "Encountered NoCredentialProviders. Retrying...\n")
            time.Sleep(time.Duration(2) * time.Second)
            continue
          }
        }
        if ! nofail {
          return err
        }
      }
      break
    }

    for _, v := range ssmVars {
      val, ok := values[v.parameter]
      if ok {
        e.os.Setenv(v.envvar, val)
      }
    }
  }

  return nil
}

func (e *expander) getParameters(names []string, decrypt bool, nofail bool) (map[string]string, error) {
  values := make(map[string]string)

  input := &ssm.GetParametersInput{
    WithDecryption: aws.Bool(decrypt),
  }

  for _, n := range names {
    input.Names = append(input.Names, aws.String(n))
  }

  resp, err := e.ssm.GetParameters(input)
  if err != nil && ! nofail {
    return values, err
  }

  if len(resp.InvalidParameters) > 0 {
    if ! nofail {
      return values, newInvalidParametersError(resp)
    }
    fmt.Fprintf(os.Stderr, "ssm-env: %v\n", newInvalidParametersError(resp))
  }

  for _, p := range resp.Parameters {
    var name string
    if p.Selector != nil {
      name = *p.Name + *p.Selector
    } else {
      name = *p.Name
    }
    values[name] = *p.Value
  }

  for _, n := range names {
    if _, ok := values[n]; ! ok {
      values[n] = ""
    }
  }

  return values, nil
}

type invalidParametersError struct {
  InvalidParameters []string
}

func newInvalidParametersError(resp *ssm.GetParametersOutput) *invalidParametersError {
  e := new(invalidParametersError)
  for _, p := range resp.InvalidParameters {
    if p == nil {
      continue
    }

    e.InvalidParameters = append(e.InvalidParameters, *p)
  }
  return e
}

func (e *invalidParametersError) Error() string {
  return fmt.Sprintf("invalid parameters: %v", e.InvalidParameters)
}

func splitVar(v string) (key, val string) {
  parts := strings.Split(v, "=")
  return parts[0], parts[1]
}

func must(err error) {
  if err != nil {
    fmt.Fprintf(os.Stderr, "ssm-env: %v\n", err)
    os.Exit(1)
  }
}

func NewHTTPClient() *http.Client {
  timeout := time.Duration(getEnv("AWS_METADATA_SERVICE_TIMEOUT", 5)) * time.Second
  tr := &http.Transport{
    ResponseHeaderTimeout: timeout,
    Proxy: http.ProxyFromEnvironment,
    DialContext: (&net.Dialer{
      KeepAlive: 30 * time.Second,
      DualStack: true,
      Timeout:   timeout,
    }).DialContext,
    MaxIdleConns:          100,
    IdleConnTimeout:       90 * time.Second,
    TLSHandshakeTimeout:   timeout,
    MaxIdleConnsPerHost:   10,
    ExpectContinueTimeout: timeout,
  }

  // So client makes HTTP/2 requests
  http2.ConfigureTransport(tr)

  return &http.Client{
      Transport: tr,
  }
}

func getEnv(key string, fallback int) int {
    value, err := strconv.Atoi(key)
    if err != nil {
      return fallback
    }
    return value
}
