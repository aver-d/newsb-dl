# newsb-dl

Simple Newsboat or Newsbeuter podcast downloader.

Downloads are concurrent but grouped by host to have a restriction that only one download is active per host.

A log of downloads is kept at `~/.config/newsb-dl/log`. The log contains tab-separated lines, each with three items: time (rfc3999), status (1 for ok, 0 for fail), and the url.

Usage:


    newsb-dl [dir]
