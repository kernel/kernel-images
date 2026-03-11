# Live View Benchmark Results

Date: Tue Mar 10 05:41:40 PM EDT 2026
Iterations: 2 (warmup: 1)

### Resource Allocation
| Type | Memory | CPUs |
|---|---|---|
| Headless | 1024m | 4 |
| Headful | 8192m | 8 |


## baseline

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    22.7ms |    22.7ms |    22.7ms |    22.7ms |    22.7ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    48.6ms |    65.4ms |   100.9ms |   305.8ms |   305.8ms |     6 |
| Screenshot.PNG                   |    62.5ms |   112.2ms |   120.3ms |   215.2ms |   215.2ms |     6 |
| Screenshot.FullPage              |    67.1ms |   462.7ms |   375.6ms |   841.0ms |   841.0ms |     6 |
| Screenshot.ClipRegion            |    24.5ms |    36.3ms |    82.6ms |   229.0ms |   229.0ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     511µs |     570µs |    12.8ms |    70.4ms |    70.4ms |     6 |
| Eval.QuerySelectorAll            |     311µs |     401µs |    12.1ms |    70.7ms |    70.7ms |     6 |
| Eval.InnerText                   |     482µs |     742µs |     1.3ms |     3.8ms |     3.8ms |     6 |
| Eval.GetComputedStyle            |     3.9ms |     4.5ms |     5.2ms |     9.8ms |     9.8ms |     6 |
| Eval.ScrollToBottom              |     284µs |     371µs |     426µs |     774µs |     774µs |     6 |
| Eval.DOMManipulation             |     497µs |     629µs |     660µs |     1.0ms |     1.0ms |     6 |
| Eval.BoundingRects               |     428µs |     829µs |     840µs |     1.5ms |     1.5ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     228µs |     295µs |     752µs |     3.1ms |     3.1ms |     6 |
| DOM.GetDocument.Deep             |     272µs |     3.0ms |     2.4ms |     5.6ms |     5.6ms |     6 |
| DOM.GetDocument.Full             |     231µs |    28.6ms |    22.5ms |    58.1ms |    58.1ms |     6 |
| DOM.QuerySelector                |     207µs |     291µs |     1.7ms |     9.1ms |     9.1ms |     6 |
| DOM.GetOuterHTML                 |     171µs |    20.8ms |    12.3ms |    27.1ms |    27.1ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     1.3ms |     6.9ms |     7.1ms |    12.6ms |    12.6ms |     6 |
| Input.Click                      |     948µs |     3.1ms |     6.9ms |    28.5ms |    28.5ms |     6 |
| Input.TypeText                   |     7.4ms |     9.8ms |    70.3ms |   201.0ms |   201.0ms |     6 |
| Input.Scroll                     |    72.7ms |    81.6ms |    95.0ms |   156.0ms |   156.0ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     400µs |     635µs |     1.1ms |     3.4ms |     3.4ms |     6 |
| Network.GetResponseBody          |     4.6ms |    66.1ms |   145.9ms |   667.6ms |   667.6ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    27.7ms |   158.3ms |   364.0ms |     1.27s |     1.27s |     6 |
| Page.GetNavigationHistory        |     1.0ms |     1.2ms |     1.5ms |     3.5ms |     3.5ms |     6 |
| Page.GetLayoutMetrics            |     390µs |     4.2ms |     9.0ms |    34.9ms |    34.9ms |     6 |
| Page.PrintToPDF                  |    30.0ms |   237.8ms |   373.9ms |     1.56s |     1.56s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     268µs |     469µs |     417µs |     548µs |     548µs |     6 |
| Emulation.SetMobile              |     1.3ms |   114.4ms |   126.1ms |   457.8ms |   457.8ms |     6 |
| Emulation.SetGeolocation         |      95µs |     173µs |     163µs |     221µs |     221µs |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     115µs |     143µs |     137µs |     150µs |     150µs |     6 |
| Target.CreateAndClose            |    10.7ms |    14.7ms |    14.9ms |    20.2ms |    20.2ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |    98.7ms |   135.3ms |   158.4ms |   282.8ms |   282.8ms |     6 |
| Composite.ScrapeLinks            |     3.5ms |     4.0ms |     3.9ms |     4.5ms |     4.5ms |     6 |
| Composite.FillForm               |     5.4ms |     7.9ms |     7.3ms |     8.8ms |     8.8ms |     6 |
| Composite.ClickAndWait           |    22.5ms |    26.3ms |    25.2ms |    27.4ms |    27.4ms |     6 |
| Composite.RapidScreenshots       |   594.7ms |   647.1ms |   643.8ms |   682.5ms |   682.5ms |     6 |
| Composite.ScrollAndCapture       |   499.3ms |   502.9ms |   505.6ms |   523.3ms |   523.3ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   106.7ms |   111.8ms |   166.3ms |   280.3ms |   280.3ms |     3 |
| Navigate[spa]                    |     1.44s |     1.44s |     1.44s |     1.44s |     1.44s |     1 |
| Navigate[media]                  |     1.52s |     1.52s |     1.52s |     1.52s |     1.52s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     200µs |     365µs |     383µs |     599µs |     927µs |   471 |
| Concurrent.Evaluate              |     192µs |    21.2ms |    20.7ms |    42.1ms |    53.6ms |   471 |
| Concurrent.Screenshot            |    56.9ms |   107.0ms |   106.8ms |   134.1ms |   148.9ms |   471 |

### baseline — Resource Usage
```
config_memory: 1024m
config_cpus: 4
image_type: headless
idle: 198.8MiB / 1GiB
after-workload: 274.2MiB / 1GiB
image_size: 2.1GB
```


## approach1

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    55.3ms |    55.3ms |    55.3ms |    55.3ms |    55.3ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    53.5ms |    85.4ms |    88.9ms |   156.9ms |   156.9ms |     6 |
| Screenshot.PNG                   |    86.7ms |   127.3ms |   194.9ms |   553.6ms |   553.6ms |     6 |
| Screenshot.FullPage              |    75.2ms |   422.1ms |   367.6ms |   993.0ms |   993.0ms |     6 |
| Screenshot.ClipRegion            |    29.0ms |    37.9ms |   116.9ms |   325.3ms |   325.3ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     402µs |     778µs |    37.6ms |   154.6ms |   154.6ms |     6 |
| Eval.QuerySelectorAll            |     332µs |     542µs |    15.0ms |    76.9ms |    76.9ms |     6 |
| Eval.InnerText                   |     441µs |     2.0ms |     3.2ms |    12.3ms |    12.3ms |     6 |
| Eval.GetComputedStyle            |     4.2ms |     5.1ms |     7.0ms |    16.1ms |    16.1ms |     6 |
| Eval.ScrollToBottom              |     352µs |     413µs |     3.7ms |    20.4ms |    20.4ms |     6 |
| Eval.DOMManipulation             |     530µs |     681µs |     1.1ms |     3.6ms |     3.6ms |     6 |
| Eval.BoundingRects               |     493µs |     997µs |     983µs |     2.1ms |     2.1ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     232µs |     275µs |     1.1ms |     5.2ms |     5.2ms |     6 |
| DOM.GetDocument.Deep             |     332µs |     3.1ms |     3.4ms |    10.8ms |    10.8ms |     6 |
| DOM.GetDocument.Full             |     456µs |    18.7ms |    21.4ms |    54.0ms |    54.0ms |     6 |
| DOM.QuerySelector                |     247µs |     336µs |     6.1ms |    35.1ms |    35.1ms |     6 |
| DOM.GetOuterHTML                 |     253µs |    22.7ms |    12.8ms |    24.9ms |    24.9ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     978µs |     9.7ms |     8.5ms |    15.7ms |    15.7ms |     6 |
| Input.Click                      |     1.1ms |     4.1ms |     6.6ms |    23.3ms |    23.3ms |     6 |
| Input.TypeText                   |     8.7ms |    10.7ms |    72.7ms |   206.9ms |   206.9ms |     6 |
| Input.Scroll                     |    71.7ms |    81.9ms |    93.6ms |   163.5ms |   163.5ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     312µs |     603µs |     4.4ms |    21.5ms |    21.5ms |     6 |
| Network.GetResponseBody          |     3.0ms |    58.6ms |    42.2ms |    69.1ms |    69.1ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    53.1ms |   176.0ms |   379.8ms |     1.13s |     1.13s |     6 |
| Page.GetNavigationHistory        |     749µs |     1.4ms |     1.4ms |     1.6ms |     1.6ms |     6 |
| Page.GetLayoutMetrics            |     378µs |     2.8ms |    23.6ms |   116.8ms |   116.8ms |     6 |
| Page.PrintToPDF                  |    31.6ms |   223.2ms |   425.2ms |     1.89s |     1.89s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     275µs |     329µs |     742µs |     2.7ms |     2.7ms |     6 |
| Emulation.SetMobile              |     1.4ms |   104.7ms |   122.8ms |   430.9ms |   430.9ms |     6 |
| Emulation.SetGeolocation         |      95µs |     179µs |     250µs |     701µs |     701µs |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |      93µs |     140µs |     149µs |     260µs |     260µs |     6 |
| Target.CreateAndClose            |    11.9ms |    19.9ms |    18.7ms |    23.0ms |    23.0ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   121.4ms |   159.0ms |   175.2ms |   266.0ms |   266.0ms |     6 |
| Composite.ScrapeLinks            |     4.0ms |     4.8ms |     4.6ms |     4.9ms |     4.9ms |     6 |
| Composite.FillForm               |     5.6ms |     8.4ms |     7.4ms |     8.7ms |     8.7ms |     6 |
| Composite.ClickAndWait           |    22.4ms |    25.5ms |    25.9ms |    31.5ms |    31.5ms |     6 |
| Composite.RapidScreenshots       |   726.7ms |   869.1ms |   829.0ms |   975.5ms |   975.5ms |     6 |
| Composite.ScrollAndCapture       |   612.2ms |   670.0ms |   656.1ms |   683.5ms |   683.5ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   118.7ms |   235.0ms |   215.4ms |   292.4ms |   292.4ms |     3 |
| Navigate[spa]                    |     1.50s |     1.50s |     1.50s |     1.50s |     1.50s |     1 |
| Navigate[media]                  |     1.13s |     1.13s |     1.13s |     1.13s |     1.13s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     226µs |     367µs |     768µs |     818µs |    29.3ms |   450 |
| Concurrent.Evaluate              |     203µs |     948µs |    10.4ms |    39.6ms |    44.6ms |   450 |
| Concurrent.Screenshot            |    69.7ms |   122.7ms |   122.3ms |   155.0ms |   184.4ms |   450 |

### approach1 — Resource Usage
```
config_memory: 1024m
config_cpus: 4
image_type: headless
idle: 268MiB / 1GiB
after-workload: 279.1MiB / 1GiB
image_size: 2.22GB
```


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

### approach2 — Resource Usage
```
config_memory: 1024m
config_cpus: 4
image_type: headless
idle: 194.7MiB / 1GiB
after-workload: 343.1MiB / 1GiB
image_size: 2.11GB
```


## headful

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    28.9ms |    28.9ms |    28.9ms |    28.9ms |    28.9ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    66.7ms |   104.5ms |   102.6ms |   187.6ms |   187.6ms |     6 |
| Screenshot.PNG                   |   133.9ms |   166.7ms |   177.9ms |   272.2ms |   272.2ms |     6 |
| Screenshot.FullPage              |   102.6ms |   395.8ms |   320.5ms |   701.5ms |   701.5ms |     6 |
| Screenshot.ClipRegion            |    76.1ms |    90.3ms |   154.8ms |   371.2ms |   371.2ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     502µs |     671µs |    14.3ms |    66.3ms |    66.3ms |     6 |
| Eval.QuerySelectorAll            |     361µs |     447µs |    14.0ms |    70.7ms |    70.7ms |     6 |
| Eval.InnerText                   |     477µs |     1.0ms |     2.6ms |    10.8ms |    10.8ms |     6 |
| Eval.GetComputedStyle            |     4.1ms |     4.5ms |     5.7ms |    10.6ms |    10.6ms |     6 |
| Eval.ScrollToBottom              |     330µs |     501µs |     666µs |     1.9ms |     1.9ms |     6 |
| Eval.DOMManipulation             |     517µs |     634µs |     682µs |     974µs |     974µs |     6 |
| Eval.BoundingRects               |     427µs |     736µs |     869µs |     1.7ms |     1.7ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     221µs |     287µs |     845µs |     3.7ms |     3.7ms |     6 |
| DOM.GetDocument.Deep             |     308µs |     2.2ms |     2.2ms |     5.6ms |     5.6ms |     6 |
| DOM.GetDocument.Full             |     250µs |    25.4ms |    21.3ms |    53.0ms |    53.0ms |     6 |
| DOM.QuerySelector                |     225µs |     317µs |     1.3ms |     6.3ms |     6.3ms |     6 |
| DOM.GetOuterHTML                 |     238µs |    23.0ms |    14.8ms |    32.3ms |    32.3ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     2.0ms |    12.0ms |    12.3ms |    23.9ms |    23.9ms |     6 |
| Input.Click                      |     1.2ms |     3.3ms |     7.1ms |    28.4ms |    28.4ms |     6 |
| Input.TypeText                   |     8.7ms |     9.8ms |    25.8ms |   100.3ms |   100.3ms |     6 |
| Input.Scroll                     |   188.5ms |   198.3ms |   345.4ms |   999.5ms |   999.5ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     260µs |     505µs |     542µs |     1.1ms |     1.1ms |     6 |
| Network.GetResponseBody          |     2.8ms |    65.8ms |    52.8ms |   108.3ms |   108.3ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    33.5ms |   163.6ms |   275.8ms |   748.8ms |   748.8ms |     6 |
| Page.GetNavigationHistory        |     796µs |     1.1ms |     1.0ms |     1.2ms |     1.2ms |     6 |
| Page.GetLayoutMetrics            |     220µs |     7.0ms |     5.7ms |    15.0ms |    15.0ms |     6 |
| Page.PrintToPDF                  |    30.4ms |   242.0ms |   252.6ms |   820.5ms |   820.5ms |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     195µs |     418µs |     371µs |     516µs |     516µs |     6 |
| Emulation.SetMobile              |     1.2ms |   101.3ms |   113.0ms |   404.6ms |   404.6ms |     6 |
| Emulation.SetGeolocation         |     128µs |     219µs |     206µs |     315µs |     315µs |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     114µs |     163µs |     191µs |     375µs |     375µs |     6 |
| Target.CreateAndClose            |    12.6ms |    17.2ms |    17.5ms |    27.2ms |    27.2ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   101.0ms |   274.3ms |   220.6ms |   289.5ms |   289.5ms |     6 |
| Composite.ScrapeLinks            |     3.8ms |     4.6ms |     4.5ms |     5.3ms |     5.3ms |     6 |
| Composite.FillForm               |     5.3ms |     6.1ms |     6.5ms |     8.1ms |     8.1ms |     6 |
| Composite.ClickAndWait           |    17.6ms |    21.7ms |    21.1ms |    24.8ms |    24.8ms |     6 |
| Composite.RapidScreenshots       |     1.04s |     1.16s |     1.13s |     1.30s |     1.30s |     6 |
| Composite.ScrollAndCapture       |   597.4ms |   639.4ms |   621.2ms |   649.3ms |   649.3ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   116.8ms |   208.1ms |   185.8ms |   232.6ms |   232.6ms |     3 |
| Navigate[spa]                    |     1.27s |     1.27s |     1.27s |     1.27s |     1.27s |     1 |
| Navigate[media]                  |   958.6ms |   958.6ms |   958.6ms |   958.6ms |   958.6ms |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     225µs |     327µs |     425µs |     661µs |    19.9ms |   391 |
| Concurrent.Evaluate              |     237µs |     504µs |     3.6ms |    19.5ms |    22.3ms |   391 |
| Concurrent.Screenshot            |    95.7ms |   151.1ms |   150.0ms |   179.5ms |   225.9ms |   391 |

### headful — Resource Usage
```
config_memory: 8192m
config_cpus: 8
image_type: headful
idle: 389.9MiB / 8GiB
after-workload: 862MiB / 8GiB
image_size: 2.66GB
```

## Side-by-side Comparison (Median)

| Operation                          |     baseline |    approach1 |    approach2 |      headful |
|------------------------------------|--------------|--------------|--------------|--------------|
| **Navigation                      ** |              |              |              |              |
| Navigate[static]                   |       22.8ms |       30.4ms |       27.2ms |       35.1ms |
| **Screenshot                      ** |              |              |              |              |
| Screenshot.JPEG.q80                |       62.5ms |       83.7ms |       57.4ms |       94.4ms |
| Screenshot.PNG                     |      108.2ms |      120.5ms |      121.0ms |      171.9ms |
| Screenshot.FullPage                |      461.4ms |      494.4ms |      446.3ms |      448.1ms |
| Screenshot.ClipRegion              |       36.6ms |       42.9ms |       38.7ms |       84.9ms |
| **JS Evaluation                   ** |              |              |              |              |
| Eval.Trivial                       |       596µs |       684µs |       473µs |       579µs |
| Eval.QuerySelectorAll              |       560µs |       469µs |       338µs |       386µs |
| Eval.InnerText                     |       964µs |        1.9ms |       739µs |        1.8ms |
| Eval.GetComputedStyle              |        5.3ms |        6.7ms |        4.9ms |        4.0ms |
| Eval.ScrollToBottom                |       376µs |       579µs |       490µs |       372µs |
| Eval.DOMManipulation               |       627µs |       872µs |       652µs |       580µs |
| Eval.BoundingRects                 |       855µs |       760µs |        4.4ms |       658µs |
| **DOM                             ** |              |              |              |              |
| DOM.GetDocument.Shallow            |       238µs |       355µs |       277µs |       219µs |
| DOM.GetDocument.Deep               |        4.2ms |        3.3ms |        5.9ms |        4.6ms |
| DOM.GetDocument.Full               |       17.8ms |       21.2ms |       22.1ms |       22.8ms |
| DOM.QuerySelector                  |       305µs |       326µs |       304µs |       314µs |
| DOM.GetOuterHTML                   |       23.0ms |       24.4ms |       24.2ms |       19.5ms |
| **Input                           ** |              |              |              |              |
| Input.MouseMove                    |        4.5ms |        8.0ms |        4.9ms |       15.6ms |
| Input.Click                        |        3.2ms |        3.2ms |        3.4ms |        3.1ms |
| Input.TypeText                     |        9.5ms |        9.9ms |        9.4ms |       11.3ms |
| Input.Scroll                       |       74.5ms |       72.9ms |       76.3ms |      198.8ms |
| **Network                         ** |              |              |              |              |
| Network.GetCookies                 |       483µs |       660µs |       394µs |       523µs |
| Network.GetResponseBody            |       65.7ms |       59.2ms |       55.0ms |       68.3ms |
| **Page                            ** |              |              |              |              |
| Page.Reload                        |      139.8ms |      146.6ms |      135.1ms |      140.4ms |
| Page.GetNavigationHistory          |        1.2ms |        1.6ms |        1.3ms |        1.5ms |
| Page.GetLayoutMetrics              |        3.6ms |        3.3ms |        3.3ms |        2.3ms |
| Page.PrintToPDF                    |      221.7ms |      225.8ms |      216.9ms |      229.7ms |
| **Emulation                       ** |              |              |              |              |
| Emulation.SetViewport              |       499µs |       322µs |       302µs |       692µs |
| Emulation.SetMobile                |      101.0ms |      105.0ms |      110.4ms |       95.4ms |
| Emulation.SetGeolocation           |       145µs |       172µs |       151µs |       212µs |
| **Target                          ** |              |              |              |              |
| Target.GetTargets                  |       124µs |       140µs |       121µs |       143µs |
| Target.CreateAndClose              |       17.4ms |       17.7ms |       16.9ms |       17.2ms |
| **Composite                       ** |              |              |              |              |
| Composite.NavAndScreenshot         |      135.7ms |      154.1ms |      156.7ms |      228.1ms |
| Composite.ScrapeLinks              |        3.9ms |        4.7ms |        3.4ms |        4.1ms |
| Composite.FillForm                 |        6.7ms |        8.0ms |        6.7ms |        7.5ms |
| Composite.ClickAndWait             |       24.6ms |       27.1ms |       26.8ms |       20.6ms |
| Composite.RapidScreenshots         |      693.2ms |      865.6ms |      691.7ms |        1.13s |
| Composite.ScrollAndCapture         |      520.8ms |      649.0ms |      503.5ms |      605.2ms |
| **Navigation                      ** |              |              |              |              |
| Navigate[content]                  |      119.4ms |      119.3ms |      214.4ms |      113.7ms |
| Navigate[spa]                      |        1.45s |        1.37s |        1.36s |        1.29s |
| Navigate[media]                    |      764.5ms |      965.9ms |      867.2ms |      790.2ms |
| **Concurrent                      ** |              |              |              |              |
| Concurrent.DOM                     |       339µs |       432µs |       328µs |       328µs |
| Concurrent.Evaluate                |       19.1ms |       19.5ms |       18.4ms |       533µs |
| Concurrent.Screenshot              |      106.3ms |      112.5ms |       98.5ms |      147.8ms |

