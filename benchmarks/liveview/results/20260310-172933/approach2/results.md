
## approach2

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    23.9ms |    23.9ms |    23.9ms |    23.9ms |    23.9ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    51.6ms |    71.8ms |   122.9ms |   392.8ms |   392.8ms |     6 |
| Screenshot.PNG                   |    79.3ms |   111.8ms |   125.9ms |   219.7ms |   219.7ms |     6 |
| Screenshot.FullPage              |    69.8ms |   478.3ms |   372.2ms |   824.2ms |   824.2ms |     6 |
| Screenshot.ClipRegion            |    24.9ms |    38.4ms |    93.9ms |   223.2ms |   223.2ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     461µs |     540µs |    12.4ms |    71.3ms |    71.3ms |     6 |
| Eval.QuerySelectorAll            |     291µs |     457µs |     483µs |     886µs |     886µs |     6 |
| Eval.InnerText                   |     461µs |     917µs |    12.9ms |    72.0ms |    72.0ms |     6 |
| Eval.GetComputedStyle            |     4.2ms |     4.8ms |     6.1ms |    12.9ms |    12.9ms |     6 |
| Eval.ScrollToBottom              |     312µs |     382µs |     405µs |     604µs |     604µs |     6 |
| Eval.DOMManipulation             |     514µs |     677µs |     765µs |     1.2ms |     1.2ms |     6 |
| Eval.BoundingRects               |     495µs |     834µs |     841µs |     1.2ms |     1.2ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     229µs |     288µs |     745µs |     3.1ms |     3.1ms |     6 |
| DOM.GetDocument.Deep             |     289µs |     2.2ms |     2.6ms |     6.4ms |     6.4ms |     6 |
| DOM.GetDocument.Full             |     269µs |    22.2ms |    21.6ms |    54.2ms |    54.2ms |     6 |
| DOM.QuerySelector                |     230µs |     338µs |     1.7ms |     8.5ms |     8.5ms |     6 |
| DOM.GetOuterHTML                 |     196µs |    21.9ms |    13.0ms |    26.4ms |    26.4ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     1.9ms |     7.3ms |     7.3ms |    13.6ms |    13.6ms |     6 |
| Input.Click                      |     1.3ms |     3.3ms |     6.3ms |    24.5ms |    24.5ms |     6 |
| Input.TypeText                   |     8.8ms |     9.4ms |    69.5ms |   215.8ms |   215.8ms |     6 |
| Input.Scroll                     |    69.9ms |    73.1ms |    94.6ms |   158.7ms |   158.7ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     299µs |     477µs |     718µs |     2.2ms |     2.2ms |     6 |
| Network.GetResponseBody          |     3.7ms |    55.4ms |    77.5ms |   286.2ms |   286.2ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    33.6ms |   153.5ms |   388.9ms |     1.17s |     1.17s |     6 |
| Page.GetNavigationHistory        |     854µs |     1.5ms |     1.5ms |     2.9ms |     2.9ms |     6 |
| Page.GetLayoutMetrics            |     392µs |     3.2ms |    23.9ms |   115.8ms |   115.8ms |     6 |
| Page.PrintToPDF                  |    37.7ms |   227.2ms |   329.0ms |     1.31s |     1.31s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     225µs |     378µs |     352µs |     557µs |     557µs |     6 |
| Emulation.SetMobile              |     1.3ms |    97.6ms |   109.8ms |   387.0ms |   387.0ms |     6 |
| Emulation.SetGeolocation         |     114µs |     184µs |     182µs |     248µs |     248µs |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     113µs |     148µs |     148µs |     176µs |     176µs |     6 |
| Target.CreateAndClose            |    14.6ms |    17.6ms |    17.2ms |    23.5ms |    23.5ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   105.5ms |   128.1ms |   170.4ms |   366.7ms |   366.7ms |     6 |
| Composite.ScrapeLinks            |     3.3ms |     3.6ms |     3.6ms |     4.0ms |     4.0ms |     6 |
| Composite.FillForm               |     5.1ms |     6.9ms |     6.5ms |     7.5ms |     7.5ms |     6 |
| Composite.ClickAndWait           |    20.9ms |    28.0ms |    25.4ms |    29.0ms |    29.0ms |     6 |
| Composite.RapidScreenshots       |   535.0ms |   683.1ms |   658.0ms |   709.5ms |   709.5ms |     6 |
| Composite.ScrollAndCapture       |   497.7ms |   538.5ms |   522.7ms |   557.3ms |   557.3ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   107.0ms |   109.5ms |   146.5ms |   223.1ms |   223.1ms |     3 |
| Navigate[spa]                    |     1.33s |     1.33s |     1.33s |     1.33s |     1.33s |     1 |
| Navigate[media]                  |     1.63s |     1.63s |     1.63s |     1.63s |     1.63s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     191µs |     362µs |     383µs |     576µs |     2.3ms |   480 |
| Concurrent.Evaluate              |     199µs |    19.7ms |    19.8ms |    41.5ms |    46.9ms |   480 |
| Concurrent.Screenshot            |    60.7ms |   105.2ms |   105.1ms |   132.6ms |   143.5ms |   480 |
