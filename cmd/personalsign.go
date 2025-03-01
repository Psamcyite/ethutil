package cmd

import (
	"crypto/ecdsa"
	"fmt"
	"log"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/spf13/cobra"
)

// personalSignCmd represents the personalSign command
var personalSignCmd = &cobra.Command{
	Use:   "personal-sign [msg]",
	Short: "Create EIP191 personal sign",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		var msg = args[0]

		if globalOptPrivateKey == "" {
			log.Fatalf("--private-key is required for this command")
		}

		privateKey := buildPrivateKeyFromHex(globalOptPrivateKey)
		sig, err := personalSign(msg, privateKey)
		checkErr(err)
		fmt.Printf("personal sign: %s, signer address: %s\n", sig, extractAddressFromPrivateKey(privateKey).String())
	},
}


// personalSign Returns personal_sign signature data
// See: https://eips.ethereum.org/EIPS/eip-191
// The signature data can be verified in https://etherscan.io/verifiedSignatures
func personalSign(message string, privateKey *ecdsa.PrivateKey) (string, error) {
	fullMessage := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)
	hash := crypto.Keccak256Hash([]byte(fullMessage))
	signatureBytes, err := crypto.Sign(hash.Bytes(), privateKey)
	if err != nil {
		return "", err
	}
	signatureBytes[64] += 27
	return hexutil.Encode(signatureBytes), nil
}
