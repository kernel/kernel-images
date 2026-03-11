
## approach1

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    98.2ms |    98.2ms |    98.2ms |    98.2ms |    98.2ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    61.9ms |   161.6ms |   281.6ms |   893.4ms |   893.4ms |     6 |
| Screenshot.PNG                   |    93.6ms |   110.4ms |   209.9ms |   602.4ms |   602.4ms |     6 |
| Screenshot.FullPage              |    75.2ms |   753.0ms |   759.8ms |     1.98s |     1.98s |     6 |
| Screenshot.ClipRegion            |    30.3ms |    44.5ms |   165.3ms |   658.1ms |   658.1ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     1.7ms |     1.8ms |    59.9ms |   349.2ms |   349.2ms |     6 |
| Eval.QuerySelectorAll            |     1.4ms |     1.6ms |    60.8ms |   355.0ms |   355.0ms |     6 |
| Eval.InnerText                   |     1.6ms |     3.3ms |    32.4ms |   130.2ms |   130.2ms |     6 |
| Eval.GetComputedStyle            |     5.9ms |    12.0ms |   146.9ms |   811.5ms |   811.5ms |     6 |
| Eval.ScrollToBottom              |     1.7ms |     4.9ms |    21.6ms |   110.1ms |   110.1ms |     6 |
| Eval.DOMManipulation             |     1.8ms |    11.5ms |    71.4ms |   396.6ms |   396.6ms |     6 |
| Eval.BoundingRects               |     1.7ms |     2.6ms |    75.9ms |   442.4ms |   442.4ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     1.4ms |     1.6ms |    13.7ms |    65.3ms |    65.3ms |     6 |
| DOM.GetDocument.Deep             |     1.6ms |     7.7ms |    11.9ms |    48.1ms |    48.1ms |     6 |
| DOM.GetDocument.Full             |     1.4ms |    42.0ms |    45.9ms |   105.7ms |   105.7ms |     6 |
| DOM.QuerySelector                |     1.6ms |     2.5ms |     5.5ms |    22.4ms |    22.4ms |     6 |
| DOM.GetOuterHTML                 |     1.4ms |    33.6ms |    21.5ms |    51.7ms |    51.7ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     1.8ms |     3.9ms |     8.7ms |    35.6ms |    35.6ms |     6 |
| Input.Click                      |     4.1ms |     5.9ms |    17.7ms |    78.5ms |    78.5ms |     6 |
| Input.TypeText                   |    59.5ms |    83.1ms |   190.4ms |   461.8ms |   461.8ms |     6 |
| Input.Scroll                     |    62.3ms |    81.7ms |    81.2ms |   119.8ms |   119.8ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     1.5ms |     1.6ms |     3.1ms |     6.8ms |     6.8ms |     6 |
| Network.GetResponseBody          |     7.8ms |    71.1ms |   127.9ms |   375.4ms |   375.4ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    46.0ms |   177.9ms |   957.7ms |     3.12s |     3.12s |     6 |
| Page.GetNavigationHistory        |     1.6ms |     7.6ms |     6.3ms |    13.2ms |    13.2ms |     6 |
| Page.GetLayoutMetrics            |     1.5ms |    14.6ms |    32.0ms |   136.6ms |   136.6ms |     6 |
| Page.PrintToPDF                  |    38.3ms |   742.4ms |     1.42s |     5.20s |     5.20s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     1.5ms |     2.5ms |     3.1ms |     7.9ms |     7.9ms |     6 |
| Emulation.SetMobile              |     9.3ms |   266.2ms |   497.1ms |     2.22s |     2.22s |     6 |
| Emulation.SetGeolocation         |     1.2ms |     1.6ms |     2.8ms |     9.5ms |     9.5ms |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     1.3ms |     2.5ms |     7.6ms |    36.5ms |    36.5ms |     6 |
| Target.CreateAndClose            |    40.4ms |    74.6ms |   100.0ms |   228.6ms |   228.6ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   128.7ms |   434.4ms |   450.6ms |     1.14s |     1.14s |     6 |
| Composite.ScrapeLinks            |     2.3ms |     3.7ms |     5.0ms |    10.9ms |    10.9ms |     6 |
| Composite.FillForm               |    12.6ms |    18.3ms |    17.6ms |    25.1ms |    25.1ms |     6 |
| Composite.ClickAndWait           |    61.0ms |    73.8ms |    70.8ms |    81.2ms |    81.2ms |     6 |
| Composite.RapidScreenshots       |   772.7ms |   899.5ms |   915.4ms |     1.08s |     1.08s |     6 |
| Composite.ScrollAndCapture       |   512.1ms |   535.7ms |   536.8ms |   569.8ms |   569.8ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   172.0ms |   295.7ms |   296.7ms |   422.3ms |   422.3ms |     3 |
| Navigate[spa]                    |     3.70s |     3.70s |     3.70s |     3.70s |     3.70s |     1 |
| Navigate[media]                  |     2.15s |     2.15s |     2.15s |     2.15s |     2.15s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     1.5ms |     2.3ms |     4.9ms |    10.0ms |   129.6ms |   311 |
| Concurrent.Evaluate              |     1.6ms |    19.1ms |    29.7ms |    74.6ms |   140.2ms |   311 |
| Concurrent.Screenshot            |    88.9ms |   155.9ms |   158.2ms |   266.9ms |   321.4ms |   311 |
