/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package cmd

import (
	"fmt"
	"time"

	"github.com/seeleteam/go-seele/seele"
	"github.com/spf13/cobra"
)

var gettps = &cobra.Command{
	Use:   "tps",
	Short: "get tps from server list",
	Long: `For example:
		tool.exe tps`,
	Run: func(cmd *cobra.Command, args []string) {
		initClient()

		for {
			sum := float64(0)
			for _, client := range clientList {
				var tps seele.TpsInfo
				err := client.Call(&tps, "debug_getTPSFromAllChains")
				if err != nil {
					fmt.Println("failed to get tps ", err)
					return
				}

				shard := getShard(client)
				for i := 0; i < seele.NumOfChains; i++ {
					fmt.Printf("shard:%d, chainNum:%d, interval:%d\n", shard, i, tps.Duration[i])
					if tps.Duration[i] > 0 {
						t := tps.Tps[i]
						fmt.Printf("shard:%d, chainNum:%d, tx count:%d, interval:%d, tps:%.2f\n", shard, i,
						tps.Count[i], tps.Duration[i], t)	
						sum += t
					}
				}	
			}

			fmt.Printf("sum tps is %.2f\n", sum)
			time.Sleep(10 * time.Second)
		}
	},
}

func init() {
	rootCmd.AddCommand(gettps)
}
