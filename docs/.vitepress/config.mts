import { defineConfig } from "vitepress";

export default defineConfig({
  title: "k8s-failover-manager",
  description:
    "Kubernetes operator for automated DNS-based service failover between clusters",
  base: "/k8s-failover-manager/",

  head: [["link", { rel: "icon", href: "/k8s-failover-manager/favicon.ico" }]],

  themeConfig: {
    nav: [
      { text: "Guide", link: "/guide/getting-started" },
      { text: "Reference", link: "/reference/crd" },
      { text: "Operations", link: "/operations/monitoring" },
    ],

    sidebar: {
      "/guide/": [
        {
          text: "Introduction",
          items: [
            { text: "What is k8s-failover-manager?", link: "/guide/overview" },
            { text: "Getting Started", link: "/guide/getting-started" },
            { text: "Architecture", link: "/guide/architecture" },
          ],
        },
        {
          text: "Core Concepts",
          items: [
            { text: "Actions", link: "/guide/actions" },
            {
              text: "Connection Killer",
              link: "/guide/connection-killer",
            },
            { text: "Remote Clusters", link: "/guide/remote-clusters" },
          ],
        },
      ],
      "/reference/": [
        {
          text: "Reference",
          items: [
            { text: "CRD Specification", link: "/reference/crd" },
            { text: "Helm Chart", link: "/reference/helm" },
            { text: "Metrics", link: "/reference/metrics" },
          ],
        },
      ],
      "/operations/": [
        {
          text: "Operations",
          items: [
            { text: "Monitoring", link: "/operations/monitoring" },
            { text: "Troubleshooting", link: "/operations/troubleshooting" },
          ],
        },
      ],
    },

    socialLinks: [
      {
        icon: "github",
        link: "https://github.com/zyno-io/k8s-failover-manager",
      },
    ],

    search: {
      provider: "local",
    },

    editLink: {
      pattern:
        "https://github.com/zyno-io/k8s-failover-manager/edit/main/docs/:path",
      text: "Edit this page on GitHub",
    },
  },
});
