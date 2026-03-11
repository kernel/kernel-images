
## approach2

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    95.4ms |    95.4ms |    95.4ms |    95.4ms |    95.4ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    31.2ms |   167.9ms |   299.7ms |   905.6ms |   905.6ms |     6 |
| Screenshot.PNG                   |    55.2ms |   171.2ms |   420.4ms |     1.24s |     1.24s |     6 |
| Screenshot.FullPage              |    65.8ms |   607.1ms |   792.9ms |     2.55s |     2.55s |     6 |
| Screenshot.ClipRegion            |    22.2ms |    37.1ms |   112.6ms |   429.0ms |   429.0ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     1.6ms |     2.8ms |     4.4ms |    14.7ms |    14.7ms |     6 |
| Eval.QuerySelectorAll            |     1.6ms |     1.6ms |     1.8ms |     2.5ms |     2.5ms |     6 |
| Eval.InnerText                   |     1.7ms |     2.8ms |    18.6ms |    94.0ms |    94.0ms |     6 |
| Eval.GetComputedStyle            |     6.3ms |     7.5ms |    13.9ms |    31.1ms |    31.1ms |     6 |
| Eval.ScrollToBottom              |     1.7ms |     3.1ms |     4.2ms |     9.2ms |     9.2ms |     6 |
| Eval.DOMManipulation             |     1.6ms |     2.7ms |     3.4ms |     7.9ms |     7.9ms |     6 |
| Eval.BoundingRects               |     1.8ms |     2.4ms |     3.1ms |     5.2ms |     5.2ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     1.5ms |     2.1ms |     2.0ms |     3.0ms |     3.0ms |     6 |
| DOM.GetDocument.Deep             |     1.6ms |     6.5ms |    11.8ms |    51.1ms |    51.1ms |     6 |
| DOM.GetDocument.Full             |     1.4ms |    71.6ms |    62.5ms |   173.7ms |   173.7ms |     6 |
| DOM.QuerySelector                |     1.7ms |     2.0ms |     8.2ms |    39.7ms |    39.7ms |     6 |
| DOM.GetOuterHTML                 |     1.7ms |    34.8ms |    43.0ms |   160.8ms |   160.8ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     2.5ms |    13.2ms |    20.7ms |    83.7ms |    83.7ms |     6 |
| Input.Click                      |     3.9ms |     6.0ms |    31.6ms |   153.2ms |   153.2ms |     6 |
| Input.TypeText                   |    63.6ms |    77.0ms |   428.8ms |     1.96s |     1.96s |     6 |
| Input.Scroll                     |    61.5ms |    71.8ms |   293.2ms |     1.36s |     1.36s |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     1.4ms |     2.1ms |     2.6ms |     5.9ms |     5.9ms |     6 |
| Network.GetResponseBody          |     7.6ms |    71.1ms |   100.5ms |   287.7ms |   287.7ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    35.0ms |   160.4ms |   932.7ms |     3.14s |     3.14s |     6 |
| Page.GetNavigationHistory        |     2.1ms |     5.5ms |     9.1ms |    27.3ms |    27.3ms |     6 |
| Page.GetLayoutMetrics            |     3.7ms |    15.9ms |    30.1ms |    80.2ms |    80.2ms |     6 |
| Page.PrintToPDF                  |    28.6ms |   335.4ms |   707.9ms |     2.31s |     2.31s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     1.8ms |     2.8ms |     4.3ms |    13.4ms |    13.4ms |     6 |
| Emulation.SetMobile              |     8.8ms |   124.4ms |   208.9ms |   676.9ms |   676.9ms |     6 |
| Emulation.SetGeolocation         |     1.4ms |     2.4ms |     2.3ms |     4.0ms |     4.0ms |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     1.3ms |     2.1ms |     2.7ms |     6.5ms |     6.5ms |     6 |
| Target.CreateAndClose            |    44.9ms |    85.5ms |    82.8ms |   121.3ms |   121.3ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   160.9ms |   364.1ms |   363.5ms |   679.6ms |   679.6ms |     6 |
| Composite.ScrapeLinks            |     2.6ms |     5.1ms |     5.3ms |     8.1ms |     8.1ms |     6 |
| Composite.FillForm               |    16.2ms |    25.1ms |    23.6ms |    31.9ms |    31.9ms |     6 |
| Composite.ClickAndWait           |    62.1ms |    84.0ms |    85.3ms |   117.7ms |   117.7ms |     6 |
| Composite.RapidScreenshots       |   812.2ms |   905.0ms |   886.7ms |     1.01s |     1.01s |     6 |
| Composite.ScrollAndCapture       |   504.0ms |   508.3ms |   513.6ms |   536.3ms |   536.3ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   156.7ms |   346.2ms |   290.8ms |   369.4ms |   369.4ms |     3 |
| Navigate[spa]                    |     2.18s |     2.18s |     2.18s |     2.18s |     2.18s |     1 |
| Navigate[media]                  |     1.91s |     1.91s |     1.91s |     1.91s |     1.91s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     1.4ms |     2.4ms |     5.7ms |     8.9ms |   102.3ms |   297 |
| Concurrent.Evaluate              |     1.6ms |    20.0ms |    29.6ms |    80.9ms |   173.9ms |   297 |
| Concurrent.Screenshot            |    74.6ms |   164.1ms |   167.6ms |   272.9ms |   325.3ms |   297 |
