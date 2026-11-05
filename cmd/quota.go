package cmd

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/rackctl/rackctl/internal/config"
	"github.com/rackctl/rackctl/internal/doctor"
	"github.com/rackctl/rackctl/internal/exec"
	"github.com/rackctl/rackctl/internal/preflight"
	"github.com/rackctl/rackctl/internal/ui"
)

// requestQuota files the vCPU increase a fresh account needs, before the preflight gate
// judges the quota, and refuses the apply while AWS decides it.
//
// The order is the point. The gate refuses an apply whose quota is short, and the only other
// place an increase is filed is phase 0, which runs after the gate. So a fresh account was
// refused for want of an increase that nothing would ever file, and the only ways forward
// were filing it by hand or --skip-preflight, which drops every other gate with it.
//
// Filing is a write, and preflight never writes, so it is a step of its own on the apply
// path. A request already waiting on AWS is found and named rather than filed again. Either
// way the apply refuses with the request named: the grant takes minutes to days, and a run
// that went ahead would bring nodes up into a quota that cannot hold them.
//
// It returns nil whenever the quota cannot be judged short, and leaves the verdict to the
// gate: a quota that could not be read is the gate's warning to give, and autoRequest off is
// the gate's refusal, which names the command to file it.
//
// The credentials are matched to cloud.accountId before anything is read or filed. The
// profile is ambient, and this step runs ahead of the gate's identity check, so without the
// match it filed the increase, and opened its support case, in whatever account the profile
// resolved to. Every re-run then found that stray request pending and refused over it,
// hiding the mismatch the gate exists to report. A mismatch, or an identity that could not
// be read, files nothing and leaves the refusal to the gate.
func requestQuota(ctx context.Context, cfg *config.Config, env []string, out io.Writer) error {
	if !cfg.Quotas.AutoRequestEnabled() || cfg.Quotas.VCPU <= 0 {
		return nil
	}
	run := exec.New(out)
	run.Env = env
	pe := &preflight.Env{Cfg: cfg, Run: run}

	if preflight.CheckIdentity(ctx, pe).Status != doctor.OK {
		return nil
	}
	have, err := preflight.VCPUQuota(ctx, pe)
	if err != nil || int(have) >= cfg.Quotas.VCPU {
		return nil
	}
	pending, err := preflight.PendingVCPURequest(ctx, pe)
	if err != nil {
		// Filing blind could duplicate a request already open, which AWS rejects anyway.
		return nil
	}
	if pending == nil {
		fmt.Fprintln(out, ui.Warn(fmt.Sprintf("EC2 vCPU quota is %d and this config asks for %d — filing the increase",
			int(have), cfg.Quotas.VCPU)))
		if err := run.Run(ctx, "aws", "service-quotas", "request-service-quota-increase",
			"--service-code", "ec2", "--quota-code", "L-1216C47A",
			"--desired-value", strconv.Itoa(cfg.Quotas.VCPU),
			"--region", cfg.Cloud.Region); err != nil {
			return withExit(ExitRefused, fmt.Errorf("aws service-quotas: filing the vCPU increase failed: %w. "+
				"File it by hand and re-run once AWS grants it: %s. Nothing was provisioned",
				err, preflight.VCPUIncreaseCommand(cfg)))
		}
		pending, _ = preflight.PendingVCPURequest(ctx, pe)
	}
	what := "the increase was filed and AWS has not yet listed it"
	if pending != nil {
		what = "increase " + pending.Describe()
	}
	return withExit(ExitRefused, fmt.Errorf(
		"the EC2 vCPU quota is %d and this config asks for %d; %s. AWS grants these in minutes to "+
			"days. Re-run `rackctl apply` once it is approved; `rackctl check` lists every other "+
			"gate meanwhile. Nothing was provisioned",
		int(have), cfg.Quotas.VCPU, what))
}
