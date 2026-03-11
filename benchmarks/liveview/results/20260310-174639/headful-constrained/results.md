
## headful-constrained

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    36.3ms |    36.3ms |    36.3ms |    36.3ms |    36.3ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    58.2ms |   100.2ms |   109.2ms |   218.9ms |   218.9ms |     6 |
| Screenshot.PNG                   |    94.7ms |   163.1ms |   175.6ms |   301.7ms |   301.7ms |     6 |
| Screenshot.FullPage              |   118.5ms |   497.7ms |   493.8ms |     1.58s |     1.58s |     6 |
| Screenshot.ClipRegion            |    75.6ms |   103.9ms |   188.0ms |   481.2ms |   481.2ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     492µs |     736µs |     4.2ms |    20.3ms |    20.3ms |     6 |
| Eval.QuerySelectorAll            |     342µs |     418µs |     3.5ms |    19.2ms |    19.2ms |     6 |
| Eval.InnerText                   |     441µs |     1.7ms |    15.4ms |    70.1ms |    70.1ms |     6 |
| Eval.GetComputedStyle            |     4.2ms |     5.0ms |     9.3ms |    20.9ms |    20.9ms |     6 |
| Eval.ScrollToBottom              |     330µs |     389µs |     1.1ms |     5.0ms |     5.0ms |     6 |
| Eval.DOMManipulation             |     488µs |     652µs |     752µs |     1.1ms |     1.1ms |     6 |
| Eval.BoundingRects               |     480µs |     954µs |     1.4ms |     4.0ms |     4.0ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     214µs |     276µs |     987µs |     4.7ms |     4.7ms |     6 |
| DOM.GetDocument.Deep             |     269µs |     2.1ms |     6.9ms |    32.9ms |    32.9ms |     6 |
| DOM.GetDocument.Full             |     241µs |    33.7ms |    23.9ms |    49.8ms |    49.8ms |     6 |
| DOM.QuerySelector                |     249µs |     380µs |     868µs |     2.1ms |     2.1ms |     6 |
| DOM.GetOuterHTML                 |     278µs |    18.2ms |    13.4ms |    32.0ms |    32.0ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     7.1ms |    24.9ms |    20.2ms |    27.2ms |    27.2ms |     6 |
| Input.Click                      |     1.5ms |     3.3ms |     8.7ms |    36.9ms |    36.9ms |     6 |
| Input.TypeText                   |     8.5ms |    10.9ms |    26.5ms |   100.2ms |   100.2ms |     6 |
| Input.Scroll                     |   186.7ms |   200.1ms |   347.5ms |   988.6ms |   988.6ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     313µs |     697µs |     1.3ms |     5.4ms |     5.4ms |     6 |
| Network.GetResponseBody          |     4.1ms |    66.9ms |    47.5ms |    74.5ms |    74.5ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    32.5ms |   174.8ms |   460.9ms |     1.84s |     1.84s |     6 |
| Page.GetNavigationHistory        |     833µs |     1.3ms |     1.5ms |     3.0ms |     3.0ms |     6 |
| Page.GetLayoutMetrics            |     208µs |     9.0ms |    31.9ms |   144.2ms |   144.2ms |     6 |
| Page.PrintToPDF                  |    29.4ms |   246.9ms |   719.7ms |     3.62s |     3.62s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     253µs |     397µs |     468µs |     956µs |     956µs |     6 |
| Emulation.SetMobile              |     1.3ms |   107.6ms |   122.0ms |   425.1ms |   425.1ms |     6 |
| Emulation.SetGeolocation         |     165µs |     237µs |     233µs |     322µs |     322µs |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     130µs |     214µs |     189µs |     246µs |     246µs |     6 |
| Target.CreateAndClose            |    15.6ms |    22.9ms |    22.8ms |    28.9ms |    28.9ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   136.6ms |   264.0ms |   244.4ms |   376.5ms |   376.5ms |     6 |
| Composite.ScrapeLinks            |     3.5ms |     3.7ms |     3.9ms |     4.6ms |     4.6ms |     6 |
| Composite.FillForm               |     5.7ms |     8.2ms |     7.7ms |     8.8ms |     8.8ms |     6 |
| Composite.ClickAndWait           |    17.5ms |    21.9ms |    24.6ms |    40.2ms |    40.2ms |     6 |
| Composite.RapidScreenshots       |     1.10s |     1.16s |     1.16s |     1.22s |     1.22s |     6 |
| Composite.ScrollAndCapture       |   603.1ms |   743.6ms |   704.2ms |   759.6ms |   759.6ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   110.9ms |   216.5ms |   190.8ms |   244.9ms |   244.9ms |     3 |
| Navigate[spa]                    |     1.52s |     1.52s |     1.52s |     1.52s |     1.52s |     1 |
| Navigate[media]                  |     1.15s |     1.15s |     1.15s |     1.15s |     1.15s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     196µs |     353µs |     445µs |     866µs |     3.1ms |   379 |
| Concurrent.Evaluate              |     212µs |     683µs |     6.4ms |    19.9ms |    61.1ms |   379 |
| Concurrent.Screenshot            |    87.9ms |   155.2ms |   152.2ms |   183.6ms |   214.3ms |   379 |
