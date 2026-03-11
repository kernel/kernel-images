
## baseline

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    90.9ms |    90.9ms |    90.9ms |    90.9ms |    90.9ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    35.6ms |    98.9ms |   252.5ms |   830.1ms |   830.1ms |     6 |
| Screenshot.PNG                   |    54.5ms |   101.6ms |   305.2ms |     1.23s |     1.23s |     6 |
| Screenshot.FullPage              |    78.9ms |   789.6ms |   824.7ms |     2.60s |     2.60s |     6 |
| Screenshot.ClipRegion            |    30.3ms |    39.5ms |   207.3ms |   881.7ms |   881.7ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     1.7ms |     2.2ms |    12.1ms |    58.1ms |    58.1ms |     6 |
| Eval.QuerySelectorAll            |     1.6ms |     1.7ms |     9.9ms |    51.3ms |    51.3ms |     6 |
| Eval.InnerText                   |     1.6ms |     2.4ms |     2.9ms |     7.0ms |     7.0ms |     6 |
| Eval.GetComputedStyle            |     6.3ms |     7.8ms |     8.7ms |    16.2ms |    16.2ms |     6 |
| Eval.ScrollToBottom              |     1.6ms |     1.8ms |     1.8ms |     2.6ms |     2.6ms |     6 |
| Eval.DOMManipulation             |     1.8ms |     2.0ms |     1.9ms |     2.1ms |     2.1ms |     6 |
| Eval.BoundingRects               |     1.8ms |     2.0ms |     2.3ms |     4.0ms |     4.0ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     1.6ms |     1.6ms |     2.7ms |     8.1ms |     8.1ms |     6 |
| DOM.GetDocument.Deep             |     1.6ms |     5.4ms |     9.8ms |    42.7ms |    42.7ms |     6 |
| DOM.GetDocument.Full             |     1.5ms |    49.9ms |    43.0ms |    91.6ms |    91.6ms |     6 |
| DOM.QuerySelector                |     1.6ms |     1.9ms |     4.4ms |    17.3ms |    17.3ms |     6 |
| DOM.GetOuterHTML                 |     1.4ms |    31.3ms |    20.4ms |    46.5ms |    46.5ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     2.1ms |     3.3ms |     6.9ms |    27.7ms |    27.7ms |     6 |
| Input.Click                      |     3.7ms |     6.0ms |    16.3ms |    69.7ms |    69.7ms |     6 |
| Input.TypeText                   |    63.6ms |    78.5ms |   481.5ms |     2.35s |     2.35s |     6 |
| Input.Scroll                     |    62.8ms |    80.1ms |   196.5ms |   748.3ms |   748.3ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     1.4ms |     1.7ms |     2.1ms |     4.5ms |     4.5ms |     6 |
| Network.GetResponseBody          |     8.0ms |    77.1ms |   110.0ms |   298.6ms |   298.6ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    51.8ms |   175.8ms |   683.2ms |     2.42s |     2.42s |     6 |
| Page.GetNavigationHistory        |     1.7ms |    11.2ms |     8.1ms |    14.0ms |    14.0ms |     6 |
| Page.GetLayoutMetrics            |     2.2ms |     8.8ms |    83.4ms |   433.7ms |   433.7ms |     6 |
| Page.PrintToPDF                  |    34.3ms |   358.0ms |   785.4ms |     2.48s |     2.48s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     1.5ms |     2.6ms |    26.3ms |   147.3ms |   147.3ms |     6 |
| Emulation.SetMobile              |     5.4ms |   137.8ms |   134.0ms |   291.2ms |   291.2ms |     6 |
| Emulation.SetGeolocation         |     1.3ms |     2.4ms |     2.7ms |     5.6ms |     5.6ms |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     1.3ms |     2.4ms |     2.1ms |     3.5ms |     3.5ms |     6 |
| Target.CreateAndClose            |    42.8ms |    91.7ms |    84.0ms |   119.6ms |   119.6ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   210.5ms |   357.4ms |   409.2ms |   953.2ms |   953.2ms |     6 |
| Composite.ScrapeLinks            |     2.8ms |     8.2ms |     8.2ms |    17.7ms |    17.7ms |     6 |
| Composite.FillForm               |    14.0ms |    25.5ms |    28.9ms |    65.9ms |    65.9ms |     6 |
| Composite.ClickAndWait           |    55.1ms |   102.0ms |    88.0ms |   128.5ms |   128.5ms |     6 |
| Composite.RapidScreenshots       |   752.7ms |   844.1ms |   843.9ms |   968.7ms |   968.7ms |     6 |
| Composite.ScrollAndCapture       |   499.4ms |   519.5ms |   521.3ms |   576.0ms |   576.0ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   159.3ms |   292.3ms |   293.9ms |   430.1ms |   430.1ms |     3 |
| Navigate[spa]                    |     4.13s |     4.13s |     4.13s |     4.13s |     4.13s |     1 |
| Navigate[media]                  |     2.13s |     2.13s |     2.13s |     2.13s |     2.13s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     1.5ms |     3.4ms |     5.4ms |    10.8ms |   111.5ms |   304 |
| Concurrent.Evaluate              |     1.6ms |    20.2ms |    31.0ms |    83.2ms |   132.6ms |   304 |
| Concurrent.Screenshot            |    76.5ms |   158.9ms |   161.0ms |   263.8ms |   347.8ms |   304 |
