load("//tools:defs.bzl", "go_library", "go_test")

package(licenses = ["notice"])

go_library(
    name = "arp",
    srcs = [
        "arp.go",
        "stats.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/sync",
        "//pkg/tcpip",
        "//pkg/tcpip/buffer",
        "//pkg/tcpip/header",
        "//pkg/tcpip/header/parse",
        "//pkg/tcpip/network/internal/ip",
        "//pkg/tcpip/stack",
    ],
)

go_test(
    name = "arp_test",
    size = "small",
    srcs = ["arp_test.go"],
    deps = [
        ":arp",
        "//pkg/refs",
        "//pkg/refsvfs2",
        "//pkg/tcpip",
        "//pkg/tcpip/buffer",
        "//pkg/tcpip/faketime",
        "//pkg/tcpip/header",
        "//pkg/tcpip/link/channel",
        "//pkg/tcpip/link/sniffer",
        "//pkg/tcpip/network/ipv4",
        "//pkg/tcpip/stack",
        "//pkg/tcpip/testutil",
        "@com_github_google_go_cmp//cmp:go_default_library",
        "@com_github_google_go_cmp//cmp/cmpopts:go_default_library",
    ],
)

go_test(
    name = "stats_test",
    size = "small",
    srcs = ["stats_test.go"],
    library = ":arp",
    deps = [
        "//pkg/refs",
        "//pkg/refsvfs2",
        "//pkg/tcpip",
        "//pkg/tcpip/stack",
        "//pkg/tcpip/testutil",
    ],
)
