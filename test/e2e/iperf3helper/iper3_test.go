package iperf3helper

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestIperf3(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Iperf3 helper Suite")
}

var _ = Describe("Iperf3 JSON Parser", func() {
	var (
		validFullJSON string
		result        *Iperf3Result
		err           error
	)

	BeforeEach(func() {
		validFullJSON = `{
      "start":    {
          "connected":    [{
                  "socket":    5,
                  "local_host":    "10.244.2.14",
                  "local_port":    51708,
                  "remote_host":    "172.18.0.7",
                  "remote_port":    12003
              }],
          "version":    "iperf 3.5",
          "system_info":    "Linux iperfclient-2660 6.2.15-100.fc36.x86_64 #1 SMP PREEMPT_DYNAMIC Thu May 11 16:51:53 UTC 2023 x86_64",
          "timestamp":    {
              "time":    "Wed, 23 Jul 2025 10:17:26 GMT",
              "timesecs":    1753265846
          },
          "connecting_to":    {
              "host":    "172.18.0.7",
              "port":    12003
          },
          "cookie":    "b2xebq6iqaaqnjylix2i3wjeeqmevdwyaisl",
          "tcp_mss_default":    1348,
          "sock_bufsize":    0,
          "sndbuf_actual":    16384,
          "rcvbuf_actual":    131072,
          "test_start":    {
              "protocol":    "TCP",
              "num_streams":    1,
              "blksize":    131072,
              "omit":    0,
              "duration":    50,
              "bytes":    0,
              "blocks":    0,
              "reverse":    0,
              "tos":    0
          }
      },
      "intervals":    [{
              "streams":    [{
                      "socket":    5,
                      "start":    0,
                      "end":    1.0001890659332275,
                      "seconds":    1.0001890659332275,
                      "bytes":    1321205760,
                      "bits_per_second":    10567648097.750378,
                      "retransmits":    0,
                      "snd_cwnd":    311388,
                      "rtt":    86,
                      "rttvar":    16,
                      "pmtu":    1400,
                      "omitted":    false
                  }]
          }],
      "end":    {
          "streams":    [{
                  "sender":    {
                      "socket":    5,
                      "start":    0,
                      "end":    50.000158071517944,
                      "seconds":    50.000158071517944,
                      "bytes":    83334095492,
                      "bits_per_second":    13333413126.062956,
                      "retransmits":    889,
                      "max_snd_cwnd":    2601640,
                      "max_rtt":    117,
                      "min_rtt":    62,
                      "mean_rtt":    71
                  },
                  "receiver":    {
                      "socket":    5,
                      "start":    0,
                      "end":    50.040286064147949,
                      "seconds":    50.000158071517944,
                      "bytes":    83334095492,
                      "bits_per_second":    13322720878.9609
                  }
              }],
          "sum_sent":    {
              "start":    0,
              "end":    50.000158071517944,
              "seconds":    50.000158071517944,
              "bytes":    83334095492,
              "bits_per_second":    13333413126.062956,
              "retransmits":    889
          },
          "sum_received":    {
              "start":    0,
              "end":    50.040286064147949,
              "seconds":    50.040286064147949,
              "bytes":    83334095492,
              "bits_per_second":    13322720878.9609
          },
          "cpu_utilization_percent":    {
              "host_total":    26.731040083439794,
              "host_user":    0.13144291414072176,
              "host_system":    26.599599166001397,
              "remote_total":    25.095665001689387,
              "remote_user":    0.44855781527332539,
              "remote_system":    24.647107186416061
          },
          "sender_tcp_congestion":    "cubic",
          "receiver_tcp_congestion":    "cubic"
      }
  }`
	})

	Describe("Parse output when --json flag enable", func() {
		Context("with normal test run", func() {
			BeforeEach(func() {
				result, err = ParseIperf3JSON(validFullJSON)
			})

			It("should parse successfully", func() {
				Expect(err).NotTo(HaveOccurred())
				Expect(result).NotTo(BeNil())
			})

			It("should extract sum_sent data correctly", func() {
				Expect(result.SumSent.Start).To(Equal(0.0))
				Expect(result.SumSent.End).To(Equal(50.000158071517944))
				Expect(result.SumSent.Seconds).To(Equal(50.000158071517944))
				Expect(result.SumSent.Bytes).To(Equal(int64(83334095492)))
				Expect(result.SumSent.BitsPerSecond).To(Equal(13333413126.062956))
				Expect(result.SumSent.Retransmits).To(Equal(889))
			})

			It("should extract sum_received data correctly", func() {
				Expect(result.SumReceived.Start).To(Equal(0.0))
				Expect(result.SumReceived.End).To(Equal(50.040286064147949))
				Expect(result.SumReceived.Seconds).To(Equal(50.040286064147949))
				Expect(result.SumReceived.Bytes).To(Equal(int64(83334095492)))
				Expect(result.SumReceived.BitsPerSecond).To(Equal(13322720878.9609))
				Expect(result.SumReceived.Retransmits).To(Equal(0)) // receiver doesn't have retransmits
			})

			It("should correctly determine bytes difference between send and receive", func() {
				// Both sent and received have same byte count: 83334095492
				Expect(result.IsBytesEqual).To(BeTrue())
				Expect(result.GetSendReceivedBytesDifference()).To(Equal(int64(0)))
			})
		})

		Context("with unequal sent/received bytes", func() {
			BeforeEach(func() {
				unequalJSON := `{
					"end": {
						"sum_sent": {
							"start": 0,
							"end": 10.0,
							"seconds": 10.0,
							"bytes": 1000000,
							"bits_per_second": 800000,
							"retransmits": 10
						},
						"sum_received": {
							"start": 0,
							"end": 10.0,
							"seconds": 10.0,
							"bytes": 950000,
							"bits_per_second": 760000
						}
					}
				}`
				result, err = ParseIperf3JSON(unequalJSON)
			})

			It("should parse successfully", func() {
				Expect(err).NotTo(HaveOccurred())
				Expect(result).NotTo(BeNil())
			})

			It("should detect bytes inequality", func() {
				Expect(result.IsBytesEqual).To(BeFalse())
			})

			It("should calculate correct bytes difference", func() {
				Expect(result.GetSendReceivedBytesDifference()).To(Equal(int64(50000)))
			})
		})

		Context("with zero sent bytes", func() {
			BeforeEach(func() {
				zeroSentJSON := `{
					"end": {
						"sum_sent": {
							"start": 0,
							"end": 10.0,
							"seconds": 10.0,
							"bytes": 0,
							"bits_per_second": 0,
							"retransmits": 0
						},
						"sum_received": {
							"start": 0,
							"end": 10.0,
							"seconds": 10.0,
							"bytes": 0,
							"bits_per_second": 0
						}
					}
				}`
				result, err = ParseIperf3JSON(zeroSentJSON)
			})

			It("should parse successfully", func() {
				Expect(err).NotTo(HaveOccurred())
			})

			It("should handle zero bytes correctly", func() {
				Expect(result.SumSent.Bytes).To(Equal(int64(0)))
				Expect(result.SumReceived.Bytes).To(Equal(int64(0)))
				Expect(result.IsBytesEqual).To(BeTrue())
			})
		})

		Context("with invalid JSON input", func() {
			It("should return known error for malformed JSON", func() {
				invalidJSON := `{"end": {"sum_sent": invalid json`
				result, err = ParseIperf3JSON(invalidJSON)

				Expect(err).To(HaveOccurred())
				Expect(result).To(BeNil())
			})

			It("should return error for empty JSON", func() {
				result, err = ParseIperf3JSON("")

				Expect(err).To(HaveOccurred())
				Expect(result).To(BeNil())
			})

			It("should return error for non-JSON string", func() {
				result, err = ParseIperf3JSON("not json at all")

				Expect(err).To(HaveOccurred())
				Expect(result).To(BeNil())
			})
		})
	})
})
