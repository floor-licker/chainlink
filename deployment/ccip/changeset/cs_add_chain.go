package changeset

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/smartcontractkit/chainlink-ccip/chainconfig"
	"github.com/smartcontractkit/chainlink-ccip/pkg/types/ccipocr3"
	"github.com/smartcontractkit/chainlink/deployment/ccip/changeset/internal"
	"github.com/smartcontractkit/chainlink/deployment/common/proposalutils"
	"github.com/smartcontractkit/chainlink/v2/core/capabilities/ccip/types"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/ccip/generated/ccip_home"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/keystone/generated/capabilities_registry"

	"github.com/smartcontractkit/ccip-owner-contracts/pkg/gethwrappers"
	"github.com/smartcontractkit/ccip-owner-contracts/pkg/proposal/mcms"
	"github.com/smartcontractkit/ccip-owner-contracts/pkg/proposal/timelock"

	"github.com/smartcontractkit/chainlink/deployment"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/ccip/generated/fee_quoter"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/ccip/generated/onramp"
)

// NewChainInboundChangeset generates a proposal
// to connect the new chain to the existing chains.
// TODO: doesn't implement the ChangeSet interface.
func NewChainInboundChangeset(
	e deployment.Environment,
	state CCIPOnChainState,
	homeChainSel uint64,
	newChainSel uint64,
	sources []uint64,
) (deployment.ChangesetOutput, error) {
	// Generate proposal which enables new destination (from test router) on all source chains.
	var batches []timelock.BatchChainOperation
	for _, source := range sources {
		enableOnRampDest, err := state.Chains[source].OnRamp.ApplyDestChainConfigUpdates(deployment.SimTransactOpts(), []onramp.OnRampDestChainConfigArgs{
			{
				DestChainSelector: newChainSel,
				Router:            state.Chains[source].TestRouter.Address(),
			},
		})
		if err != nil {
			return deployment.ChangesetOutput{}, err
		}
		enableFeeQuoterDest, err := state.Chains[source].FeeQuoter.ApplyDestChainConfigUpdates(
			deployment.SimTransactOpts(),
			[]fee_quoter.FeeQuoterDestChainConfigArgs{
				{
					DestChainSelector: newChainSel,
					DestChainConfig:   DefaultFeeQuoterDestChainConfig(),
				},
			})
		if err != nil {
			return deployment.ChangesetOutput{}, err
		}
		batches = append(batches, timelock.BatchChainOperation{
			ChainIdentifier: mcms.ChainIdentifier(source),
			Batch: []mcms.Operation{
				{
					// Enable the source in on ramp
					To:    state.Chains[source].OnRamp.Address(),
					Data:  enableOnRampDest.Data(),
					Value: big.NewInt(0),
				},
				{
					To:    state.Chains[source].FeeQuoter.Address(),
					Data:  enableFeeQuoterDest.Data(),
					Value: big.NewInt(0),
				},
			},
		})
	}

	addChainOp, err := applyChainConfigUpdatesOp(e, state, homeChainSel, []uint64{newChainSel})
	if err != nil {
		return deployment.ChangesetOutput{}, err
	}

	batches = append(batches, timelock.BatchChainOperation{
		ChainIdentifier: mcms.ChainIdentifier(homeChainSel),
		Batch: []mcms.Operation{
			addChainOp,
		},
	})

	var (
		timelocksPerChain = make(map[uint64]common.Address)
		proposerMCMSes    = make(map[uint64]*gethwrappers.ManyChainMultiSig)
	)
	for _, chain := range append(sources, homeChainSel) {
		timelocksPerChain[chain] = state.Chains[chain].Timelock.Address()
		proposerMCMSes[chain] = state.Chains[chain].ProposerMcm
	}
	prop, err := proposalutils.BuildProposalFromBatches(
		timelocksPerChain,
		proposerMCMSes,
		batches,
		"proposal to set new chains",
		0,
	)
	if err != nil {
		return deployment.ChangesetOutput{}, err
	}

	return deployment.ChangesetOutput{
		Proposals: []timelock.MCMSWithTimelockProposal{*prop},
	}, nil
}

// AddDonAndSetCandidateChangeset adds new DON for destination to home chain
// and sets the commit plugin config as candidateConfig for the don.
func AddDonAndSetCandidateChangeset(
	state CCIPOnChainState,
	e deployment.Environment,
	nodes deployment.Nodes,
	ocrSecrets deployment.OCRSecrets,
	homeChainSel, feedChainSel, newChainSel uint64,
	tokenConfig TokenConfig,
	pluginType types.PluginType,
) (deployment.ChangesetOutput, error) {
	ccipOCRParams := DefaultOCRParams(
		feedChainSel,
		tokenConfig.GetTokenInfo(e.Logger, state.Chains[newChainSel].LinkToken, state.Chains[newChainSel].Weth9),
		// TODO: Need USDC support.
		nil,
	)
	newDONArgs, err := internal.BuildOCR3ConfigForCCIPHome(
		ocrSecrets,
		state.Chains[newChainSel].OffRamp,
		e.Chains[newChainSel],
		nodes.NonBootstraps(),
		state.Chains[homeChainSel].RMNHome.Address(),
		ccipOCRParams.OCRParameters,
		ccipOCRParams.CommitOffChainConfig,
		ccipOCRParams.ExecuteOffChainConfig,
	)
	if err != nil {
		return deployment.ChangesetOutput{}, err
	}
	latestDon, err := internal.LatestCCIPDON(state.Chains[homeChainSel].CapabilityRegistry)
	if err != nil {
		return deployment.ChangesetOutput{}, err
	}
	commitConfig, ok := newDONArgs[pluginType]
	if !ok {
		return deployment.ChangesetOutput{}, fmt.Errorf("missing commit plugin in ocr3Configs")
	}
	donID := latestDon.Id + 1
	addDonOp, err := newDonWithCandidateOp(
		donID, commitConfig,
		state.Chains[homeChainSel].CapabilityRegistry,
		nodes.NonBootstraps(),
	)
	if err != nil {
		return deployment.ChangesetOutput{}, err
	}

	var (
		timelocksPerChain = map[uint64]common.Address{
			homeChainSel: state.Chains[homeChainSel].Timelock.Address(),
		}
		proposerMCMSes = map[uint64]*gethwrappers.ManyChainMultiSig{
			homeChainSel: state.Chains[homeChainSel].ProposerMcm,
		}
	)
	prop, err := proposalutils.BuildProposalFromBatches(
		timelocksPerChain,
		proposerMCMSes,
		[]timelock.BatchChainOperation{{
			ChainIdentifier: mcms.ChainIdentifier(homeChainSel),
			Batch:           []mcms.Operation{addDonOp},
		}},
		"setCandidate for commit and AddDon on new Chain",
		0, // minDelay
	)
	if err != nil {
		return deployment.ChangesetOutput{}, fmt.Errorf("failed to build proposal from batch: %w", err)
	}

	return deployment.ChangesetOutput{
		Proposals: []timelock.MCMSWithTimelockProposal{*prop},
	}, nil
}

func applyChainConfigUpdatesOp(
	e deployment.Environment,
	state CCIPOnChainState,
	homeChainSel uint64,
	chains []uint64,
) (mcms.Operation, error) {
	nodes, err := deployment.NodeInfo(e.NodeIDs, e.Offchain)
	if err != nil {
		return mcms.Operation{}, err
	}
	encodedExtraChainConfig, err := chainconfig.EncodeChainConfig(chainconfig.ChainConfig{
		GasPriceDeviationPPB:    ccipocr3.NewBigIntFromInt64(1000),
		DAGasPriceDeviationPPB:  ccipocr3.NewBigIntFromInt64(0),
		OptimisticConfirmations: 1,
	})
	if err != nil {
		return mcms.Operation{}, err
	}
	var chainConfigUpdates []ccip_home.CCIPHomeChainConfigArgs
	for _, chainSel := range chains {
		chainConfig := setupConfigInfo(chainSel, nodes.NonBootstraps().PeerIDs(),
			nodes.DefaultF(), encodedExtraChainConfig)
		chainConfigUpdates = append(chainConfigUpdates, chainConfig)
	}

	addChain, err := state.Chains[homeChainSel].CCIPHome.ApplyChainConfigUpdates(
		deployment.SimTransactOpts(),
		nil,
		chainConfigUpdates,
	)
	if err != nil {
		return mcms.Operation{}, err
	}
	return mcms.Operation{
		To:    state.Chains[homeChainSel].CCIPHome.Address(),
		Data:  addChain.Data(),
		Value: big.NewInt(0),
	}, nil
}

// newDonWithCandidateOp sets the candidate commit config by calling setCandidate on CCIPHome contract through the AddDON call on CapReg contract
// This should be done first before calling any other UpdateDON calls
// This proposes to set up OCR3 config for the commit plugin for the DON
func newDonWithCandidateOp(
	donID uint32,
	pluginConfig ccip_home.CCIPHomeOCR3Config,
	capReg *capabilities_registry.CapabilitiesRegistry,
	nodes deployment.Nodes,
) (mcms.Operation, error) {
	encodedSetCandidateCall, err := internal.CCIPHomeABI.Pack(
		"setCandidate",
		donID,
		pluginConfig.PluginType,
		pluginConfig,
		[32]byte{},
	)
	if err != nil {
		return mcms.Operation{}, fmt.Errorf("pack set candidate call: %w", err)
	}
	addDonTx, err := capReg.AddDON(deployment.SimTransactOpts(), nodes.PeerIDs(), []capabilities_registry.CapabilitiesRegistryCapabilityConfiguration{
		{
			CapabilityId: internal.CCIPCapabilityID,
			Config:       encodedSetCandidateCall,
		},
	}, false, false, nodes.DefaultF())
	if err != nil {
		return mcms.Operation{}, fmt.Errorf("could not generate add don tx w/ commit config: %w", err)
	}
	return mcms.Operation{
		To:    capReg.Address(),
		Data:  addDonTx.Data(),
		Value: big.NewInt(0),
	}, nil
}