package execution

import (
	"fmt"

	"github.com/thetatoken/ukulele/common"
	"github.com/thetatoken/ukulele/common/result"
	"github.com/thetatoken/ukulele/core"
	"github.com/thetatoken/ukulele/ledger/types"
)

var _ TxExecutor = (*SlashTxExecutor)(nil)

// ------------------------------- Slash Transaction -----------------------------------

type SlashTxExecutor struct {
	consensus core.ConsensusEngine
	valMgr    core.ValidatorManager
}

// NewSlashTxExecutor creates a new instance of SlashTxExecutor
func NewSlashTxExecutor(consensus core.ConsensusEngine, valMgr core.ValidatorManager) *SlashTxExecutor {
	return &SlashTxExecutor{
		consensus: consensus,
	}
}

func (exec *SlashTxExecutor) sanityCheck(chainID string, view types.ViewDataGetter, transaction types.Tx) result.Result {
	tx := transaction.(*types.SlashTx)

	validatorAddresses := getValidatorAddresses(exec.consensus, exec.valMgr)

	// Validate proposer, basic
	res := tx.Proposer.ValidateBasic()
	if res.IsError() {
		return res
	}

	// verify the proposer is one of the validators
	res = isAValidator(tx.Proposer.PubKey, validatorAddresses)
	if res.IsError() {
		return res
	}

	proposerAccount, res := getInput(view, tx.Proposer)
	if res.IsError() {
		return res
	}

	// verify the proposer's signature
	signBytes := tx.SignBytes(chainID)
	if !proposerAccount.PubKey.VerifySignature(signBytes, tx.Proposer.Signature) {
		return result.Error("SignBytes: %X", signBytes)
	}

	slashedAddress := tx.SlashedAddress
	slashedAccount := view.GetAccount(slashedAddress)
	if slashedAccount == nil {
		return result.Error("Account %v does not exist!", slashedAddress)
	}

	if slashedAccount.PubKey.IsEmpty() {
		return result.Error("Account %v's Pubkey is not known yet!", slashedAddress)
	}

	reservedFundFound := false
	for _, reservedFund := range slashedAccount.ReservedFunds {
		if reservedFund.ReserveSequence == tx.ReserveSequence {
			reservedFundFound = true
			break
		}
	}

	if !reservedFundFound {
		return result.Error("Reserved fund not found for %v", tx.ReserveSequence)
	}

	validatorAddress := tx.Proposer.PubKey.Address()
	validatorAccount := view.GetAccount(validatorAddress)
	if validatorAccount == nil {
		return result.Error("Validator %v does not exist!", validatorAddress)
	}

	// TODO: Add a check that validatorAccount is indeed a validator (check against the current validator list)

	overspendingProofBytes := tx.SlashProof
	slashProofVerified := exec.verifySlashProof(chainID, slashedAccount, overspendingProofBytes)
	if !slashProofVerified {
		return result.Error("Invalid slash proof: %v", overspendingProofBytes)
	}

	return result.OK
}

func (exec *SlashTxExecutor) process(chainID string, view types.ViewDataAccessor, transaction types.Tx) (common.Hash, result.Result) {
	tx := transaction.(*types.SlashTx)

	slashedAddress := tx.SlashedAddress
	slashedAccount := view.GetAccount(slashedAddress)

	var reservedFundIdx int
	var reservedFund types.ReservedFund
	reservedFundFound := false
	for reservedFundIdx, reservedFund = range slashedAccount.ReservedFunds {
		if reservedFund.ReserveSequence == tx.ReserveSequence {
			reservedFundFound = true
			break
		}
	}

	if !reservedFundFound {
		return common.Hash{}, result.Error("Reserved fund not found for %v", tx.ReserveSequence)
	}

	proposerAddress := tx.Proposer.PubKey.Address()
	proposerAccount := view.GetAccount(proposerAddress)
	if proposerAccount == nil {
		return common.Hash{}, result.Error("Proposer %v does not exist!", proposerAddress)
	}

	// TODO: We should transfer the collateral to a special address, e.g. 0x0 instead of
	//       transfering to the validator, so the validator gain no extra benefit if it colludes with
	//       the address that overspent

	// Slash: transfer the collateral and remainding deposit to the validator that identified the overspending
	remainingFund := reservedFund.InitialFund.Minus(reservedFund.UsedFund)
	if !remainingFund.IsNonnegative() {
		remainingFund = types.Coins{} // Should NOT happen, just to be on the safe side
	}
	slashedAmount := reservedFund.Collateral.Plus(remainingFund)

	proposerAccount.Balance = proposerAccount.Balance.Plus(slashedAmount)
	slashedAccount.ReservedFunds = append(slashedAccount.ReservedFunds[:reservedFundIdx],
		slashedAccount.ReservedFunds[reservedFundIdx+1:]...)

	view.SetAccount(proposerAddress, proposerAccount)
	view.SetAccount(slashedAddress, slashedAccount)

	txHash := types.TxID(chainID, tx)
	return txHash, result.OK
}

func (exec *SlashTxExecutor) verifySlashProof(chainID string, slashedAccount *types.Account, overspendingProofBytes []byte) bool {
	var overspendingProof types.OverspendingProof
	err := types.FromBytes(overspendingProofBytes, &overspendingProof)
	if err != nil {
		// TODO: need proper logging and error handling here.
		panic(fmt.Sprintf("Failed to parse overspending proof: %v\n", err))
	}

	slashedAddress := slashedAccount.PubKey.Address()
	reserveSequence := overspendingProof.ReserveSequence
	for _, reservedFund := range slashedAccount.ReservedFunds {
		if reservedFund.ReserveSequence != reserveSequence {
			continue
		}

		settledPaymentLookup := make(map[string]bool)
		fundIntendedToSpend := types.Coins{}
		for _, servicePaymentTx := range overspendingProof.ServicePayments {
			if slashedAddress == servicePaymentTx.Source.Address {
				return false // servicePaymentTx does not come from the slashed account
			}

			if servicePaymentTx.ReserveSequence != overspendingProof.ReserveSequence {
				return false // servicePaymentTx does not belong to claimed reserved fund
			}

			sourceSignedBytes := servicePaymentTx.SourceSignBytes(chainID)
			if !slashedAccount.PubKey.VerifySignature(sourceSignedBytes, servicePaymentTx.Source.Signature) {
				return false // servicePaymentTx not signed by the slashed account
			}

			paymentKey := string(servicePaymentTx.Target.Address[:]) + "." + string(servicePaymentTx.PaymentSequence)
			_, targetExists := settledPaymentLookup[paymentKey]
			if targetExists {
				return false // to prevent using partial payments as proof
			}
			settledPaymentLookup[paymentKey] = true

			fundIntendedToSpend = fundIntendedToSpend.Plus(servicePaymentTx.Source.Coins)
		}

		fundOverspent := !reservedFund.InitialFund.IsGTE(fundIntendedToSpend)
		return fundOverspent
	}

	return false
}
