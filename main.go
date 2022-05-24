package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path"
	"strconv"
	"sync"
	"syscall"

	"github.com/nscuro/dtrack-client"
	"github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
	"github.com/peterbourgon/ff/v3/ffyaml"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"

	"github.com/nscuro/dtapac/internal/api"
	"github.com/nscuro/dtapac/internal/audit"
	"github.com/nscuro/dtapac/internal/model"
	"github.com/nscuro/dtapac/internal/opa"
)

func main() {
	fs := flag.NewFlagSet("dtapac", flag.ContinueOnError)
	fs.String("config", "", "Path to config file")

	var opts options
	fs.StringVar(&opts.Host, "host", "0.0.0.0", "Host to listen on")
	fs.UintVar(&opts.Port, "port", 8080, "Port to listen on")
	fs.StringVar(&opts.DTrackURL, "dtrack-url", "", "Dependency-Track API server URL")
	fs.StringVar(&opts.DTrackAPIKey, "dtrack-apikey", "", "Dependency-Track API key")
	fs.StringVar(&opts.OPAURL, "opa-url", "", "Open Policy Agent URL")
	fs.StringVar(&opts.WatchBundle, "watch-bundle", "", "OPA bundle to watch")
	fs.StringVar(&opts.FindingPolicyPath, "finding-policy-path", "", "Policy path for finding analysis")
	fs.StringVar(&opts.ViolationPolicyPath, "violation-policy-path", "", "Policy path for violation analysis")

	cmd := ffcli.Command{
		Name: "dtapac",
		Options: []ff.Option{
			ff.WithEnvVarNoPrefix(),
			ff.WithConfigFileFlag("config"),
			ff.WithConfigFileParser(ffyaml.Parser),
			ff.WithAllowMissingConfigFile(true),
		},
		FlagSet: fs,
		Exec: func(ctx context.Context, _ []string) error {
			return exec(ctx, opts)
		},
	}

	err := cmd.ParseAndRun(context.Background(), os.Args[1:])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type options struct {
	Host                string
	Port                uint
	DTrackURL           string
	DTrackAPIKey        string
	OPAURL              string
	WatchBundle         string
	FindingPolicyPath   string
	ViolationPolicyPath string
}

func exec(ctx context.Context, opts options) error {
	logger := log.Output(zerolog.ConsoleWriter{
		Out: os.Stderr,
	})

	dtClient, err := dtrack.NewClient(opts.DTrackURL, dtrack.WithAPIKey(opts.DTrackAPIKey))
	if err != nil {
		return fmt.Errorf("failed to setup dtrack client: %w", err)
	}

	opaClient, err := opa.NewClient(opts.OPAURL)
	if err != nil {
		return fmt.Errorf("failed to setup opa client: %w", err)
	}

	var (
		findingAuditor   audit.FindingAuditor
		violationAuditor audit.ViolationAuditor
	)
	if opts.FindingPolicyPath != "" {
		findingAuditor = func(finding model.Finding) (analysis model.FindingAnalysis, auditErr error) {
			auditErr = opaClient.Decision(context.Background(), path.Join(opts.FindingPolicyPath, "/analysis"), finding, &analysis)
			return
		}
	}
	if opts.ViolationPolicyPath != "" {
		violationAuditor = func(violation model.Violation) (analysis model.ViolationAnalysis, auditErr error) {
			auditErr = opaClient.Decision(context.Background(), path.Join(opts.ViolationPolicyPath, "/analysis"), violation, &analysis)
			return
		}
	}
	if findingAuditor == nil && violationAuditor == nil {
		return fmt.Errorf("neither findings- nor violations analysis path provided")
	}

	apiServerAddr := net.JoinHostPort(opts.Host, strconv.FormatUint(uint64(opts.Port), 10))
	apiServer := api.NewServer(apiServerAddr, findingAuditor, violationAuditor, serviceLogger("apiServer", logger))

	// Audit results can come from multiple sources (ad-hoc or portfolio-wide analyses).
	// We keep track of them in a slice and merge them later if necessary.
	auditChans := []<-chan any{apiServer.AuditChan()}

	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(apiServer.Start)

	if opts.WatchBundle != "" {
		bundleWatcher := opa.NewBundleWatcher(opts.WatchBundle, apiServer.OPAStatusChan(), serviceLogger("bundleWatcher", logger))

		// Listen for bundle updates and trigger a portfolio-wide analysis
		// if an update was received.
		triggerChan := make(chan struct{}, 1)
		eg.Go(func() error {
			defer close(triggerChan)

			for revision := range bundleWatcher.Subscribe() {
				logger.Info().
					Str("bundle", opts.WatchBundle).
					Str("revision", revision).
					Msg("bundle updated")
				select {
				case triggerChan <- struct{}{}:
				default:
				}
			}

			return nil
		})

		// Listen for triggers from the above goroutine and perform a portfolio-wide
		// analysis when triggered.
		auditChan := make(chan any, 1)
		auditChans = append(auditChans, auditChan)
		eg.Go(func() error {
			defer close(auditChan)

			for range triggerChan {
				logger.Info().Msg("starting portfolio analysis")

				projects, err := dtrack.FetchAll(func(po dtrack.PageOptions) (dtrack.Page[dtrack.Project], error) {
					return dtClient.Project.GetAll(egCtx, po)
				})
				if err != nil {
					logger.Error().Err(err).Msg("failed to fetch projects")
					continue
				}

				for i, project := range projects {
					if findingAuditor != nil {
						findings, err := dtrack.FetchAll(func(po dtrack.PageOptions) (dtrack.Page[dtrack.Finding], error) {
							return dtClient.Finding.GetAll(egCtx, project.UUID, true, po)
						})
						if err != nil {
							logger.Error().Err(err).
								Str("project", project.UUID.String()).
								Msg("failed to fetch findings")
							continue
						}

						for j := range findings {
							finding := model.Finding{
								Component:     findings[j].Component,
								Project:       projects[i],
								Vulnerability: findings[j].Vulnerability,
							}
							analysis, err := findingAuditor(finding)
							if err != nil {
								logger.Error().Err(err).
									Object("finding", finding).
									Msg("failed to audit finding")
								continue
							}
							if analysis != (model.FindingAnalysis{}) {
								auditChan <- dtrack.AnalysisRequest{
									Component:     finding.Component.UUID,
									Project:       finding.Project.UUID,
									Vulnerability: finding.Vulnerability.UUID,
									State:         analysis.State,
									Justification: analysis.Justification,
									Response:      analysis.Response,
									Comment:       analysis.Comment,
									Suppressed:    analysis.Suppress,
								}
							}
						}
					} else {
						logger.Warn().Msg("findings auditing is disabled, skipping")
					}

					if violationAuditor != nil {
						violations, err := dtrack.FetchAll(func(po dtrack.PageOptions) (dtrack.Page[dtrack.PolicyViolation], error) {
							return dtClient.PolicyViolation.GetAllForProject(egCtx, project.UUID, false, po)
						})
						if err != nil {
							logger.Error().Err(err).
								Str("project", project.UUID.String()).
								Msg("failed to fetch policy violations")
							continue
						}

						for range violations {
							violation := model.Violation{}
							analysis, err := violationAuditor(violation)
							if err != nil {
								logger.Error().Err(err).
									Object("violation", violation).
									Msg("failed to audit violation")
								continue
							}
							if analysis != (model.ViolationAnalysis{}) {
								auditChan <- dtrack.ViolationAnalysisRequest{
									// TODO: Component:       subject.Component.UUID,
									// TODO: PolicyViolation: subject.PolicyViolation.UUID,
									State:      analysis.State,
									Comment:    analysis.Comment,
									Suppressed: analysis.Suppress,
								}
							}
						}
					} else {
						logger.Warn().Msg("violations auditing is disabled, skipping")
					}
				}

				logger.Info().Msg("portfolio analysis completed")
			}

			return nil
		})

		eg.Go(func() error {
			return bundleWatcher.Start(egCtx)
		})
	}

	// Merge multiple audit result channels into one.
	// Submitting audit results must be a non-concurrent operation,
	// otherwise we'll be running in all kinds of weird race conditions.
	var auditChan <-chan any
	if len(auditChans) == 1 {
		auditChan = auditChans[0]
	} else {
		auditChan = merge(auditChans...)
	}

	submitter := audit.NewSubmitter(dtClient.Analysis, serviceLogger("submitter", logger))
	eg.Go(func() error {
		for auditResult := range auditChan {
			switch res := auditResult.(type) {
			case dtrack.AnalysisRequest:
				submitErr := submitter.SubmitAnalysis(egCtx, res)
				if submitErr != nil {
					logger.Error().Err(submitErr).
						Interface("request", res).
						Msg("failed to submit analysis request")
				}
			case dtrack.ViolationAnalysisRequest:
				submitErr := submitter.SubmitViolationAnalysis(egCtx, res)
				if submitErr != nil {
					logger.Error().Err(submitErr).
						Interface("request", res).
						Msg("failed to submit violation analysis request")
				}
			}
		}

		return nil
	})

	termChan := make(chan os.Signal, 1)
	signal.Notify(termChan, os.Interrupt, syscall.SIGTERM)
	select {
	case <-termChan:
		logger.Debug().Str("reason", "shutdown requested").Msg("shutting down")
	case <-egCtx.Done():
		logger.Debug().AnErr("reason", egCtx.Err()).Msg("shutting down")
	}

	err = apiServer.Stop()
	if err != nil {
		logger.Error().Err(err).Msg("failed to shutdown api server")
	}

	return eg.Wait()
}

func serviceLogger(name string, parent zerolog.Logger) zerolog.Logger {
	return parent.With().Str("svc", name).Logger()
}

// merge converts a list of channels to a single channel, implementing a fan-in operation.
//
// This code snippet was taken from https://go.dev/blog/pipelines
func merge(cs ...<-chan any) <-chan any {
	var wg sync.WaitGroup
	out := make(chan any, 1)

	// Start an output goroutine for each input channel in cs.  output
	// copies values from c to out until c is closed, then calls wg.Done.
	output := func(c <-chan any) {
		for n := range c {
			out <- n
		}
		wg.Done()
	}
	wg.Add(len(cs))
	for _, c := range cs {
		go output(c)
	}

	// Start a goroutine to close out once all the output goroutines are
	// done.  This must start after the wg.Add call.
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
