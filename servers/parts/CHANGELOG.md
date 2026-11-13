# parts changelog

## 0.1.0

First release.

- Search DigiKey, Mouser and JLCPCB's parts library (through jlcsearch) at once, with offers grouped by manufacturer part number and the cheapest offer with enough stock at a quantity.
- Look up one part by manufacturer or distributor part number, with specifications.
- Each source is enabled only when its credentials are set; jlcsearch needs none.
- Answers are cached, and each source has a rate and a daily budget below the distributor's, so one shared instance can serve everyone.
