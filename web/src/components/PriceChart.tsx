import React, { useEffect, useRef } from 'react';
import {
  createChart,
  ColorType,
  type IChartApi,
  type ISeriesApi,
  type UTCTimestamp,
} from 'lightweight-charts';
import type { Candle } from '../types';

export type CandleInterval = '1m' | '5m' | '15m' | '1h';

interface PriceChartProps {
  symbol: string;
  interval: CandleInterval;
  onChangeInterval: (interval: CandleInterval) => void;
  candles: Candle[];
  liveCandle?: Candle;
  theme?: 'light' | 'dark';
}

const INTERVALS: { id: CandleInterval; label: string }[] = [
  { id: '1m', label: '1m' },
  { id: '5m', label: '5m' },
  { id: '15m', label: '15m' },
  { id: '1h', label: '1h' },
];

export const PriceChart: React.FC<PriceChartProps> = ({
  symbol,
  interval,
  onChangeInterval,
  candles,
  liveCandle,
  theme = 'light',
}) => {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const seriesRef = useRef<ISeriesApi<'Candlestick'> | null>(null);
  const volSeriesRef = useRef<ISeriesApi<'Histogram'> | null>(null);

  const isDark = theme === 'dark';

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    const bgColor = isDark ? '#181a20' : '#ffffff';
    const textColor = isDark ? '#9ca3af' : '#7c7e8c';
    const gridColor = isDark ? 'rgba(255, 255, 255, 0.04)' : '#f3f4f6';
    const borderColor = isDark ? '#2a2d37' : '#ebedf0';

    const chart = createChart(el, {
      layout: {
        background: { type: ColorType.Solid, color: bgColor },
        textColor,
        fontFamily: "'Inter', -apple-system, BlinkMacSystemFont, sans-serif",
      },
      grid: {
        vertLines: { color: gridColor },
        horzLines: { color: gridColor },
      },
      timeScale: {
        timeVisible: true,
        secondsVisible: false,
        borderColor,
      },
      rightPriceScale: {
        borderColor,
      },
      crosshair: {
        vertLine: {
          color: isDark ? 'rgba(0, 208, 156, 0.4)' : 'rgba(0, 179, 134, 0.35)',
          labelBackgroundColor: '#00b386',
        },
        horzLine: {
          color: isDark ? 'rgba(0, 208, 156, 0.4)' : 'rgba(0, 179, 134, 0.35)',
          labelBackgroundColor: '#00b386',
        },
      },
      width: el.clientWidth || 680,
      height: 330,
    });

    const candleSeries = chart.addCandlestickSeries({
      upColor: '#00b386',
      downColor: '#eb5b3c',
      borderVisible: false,
      wickUpColor: '#00b386',
      wickDownColor: '#eb5b3c',
    });

    candleSeries.priceScale().applyOptions({
      scaleMargins: {
        top: 0.08,
        bottom: 0.24,
      },
    });

    const volumeSeries = chart.addHistogramSeries({
      priceFormat: { type: 'volume' },
      priceScaleId: '',
    });

    volumeSeries.priceScale().applyOptions({
      scaleMargins: {
        top: 0.78,
        bottom: 0,
      },
    });

    chartRef.current = chart;
    seriesRef.current = candleSeries;
    volSeriesRef.current = volumeSeries;

    const onResize = () => {
      if (containerRef.current && chartRef.current) {
        chartRef.current.applyOptions({ width: containerRef.current.clientWidth });
      }
    };
    window.addEventListener('resize', onResize);

    return () => {
      window.removeEventListener('resize', onResize);
      chart.remove();
      chartRef.current = null;
      seriesRef.current = null;
      volSeriesRef.current = null;
    };
  }, [isDark]);

  useEffect(() => {
    if (!seriesRef.current || !volSeriesRef.current || !candles.length) return;
    const formattedCandles = candles.map((c) => ({
      time: c.bucket_start as UTCTimestamp,
      open: c.open / 100,
      high: c.high / 100,
      low: c.low / 100,
      close: c.close / 100,
    }));
    const formattedVol = candles.map((c) => ({
      time: c.bucket_start as UTCTimestamp,
      value: c.volume,
      color:
        c.close >= c.open
          ? 'rgba(0, 179, 134, 0.32)'
          : 'rgba(235, 91, 60, 0.32)',
    }));
    seriesRef.current.setData(formattedCandles);
    volSeriesRef.current.setData(formattedVol);
    chartRef.current?.timeScale().fitContent();
  }, [symbol, interval, candles, isDark]);

  useEffect(() => {
    if (
      !seriesRef.current ||
      !volSeriesRef.current ||
      !liveCandle ||
      liveCandle.symbol !== symbol ||
      (liveCandle.interval && liveCandle.interval !== interval)
    ) {
      return;
    }
    seriesRef.current.update({
      time: liveCandle.bucket_start as UTCTimestamp,
      open: liveCandle.open / 100,
      high: liveCandle.high / 100,
      low: liveCandle.low / 100,
      close: liveCandle.close / 100,
    });
    volSeriesRef.current.update({
      time: liveCandle.bucket_start as UTCTimestamp,
      value: liveCandle.volume,
      color:
        liveCandle.close >= liveCandle.open
          ? 'rgba(0, 179, 134, 0.32)'
          : 'rgba(235, 91, 60, 0.32)',
    });
  }, [symbol, interval, liveCandle, isDark]);

  return (
    <div className="groww-chart-wrapper">
      <div ref={containerRef} className="chart-canvas" />

      <div className="groww-chart-toolbar">
        <div className="groww-exchange-badge">OFX · EQ · {symbol}</div>

        <div className="groww-timeframe-pills">
          {INTERVALS.map((tf) => (
            <button
              key={tf.id}
              type="button"
              className={`groww-tf-pill ${interval === tf.id ? 'active' : ''}`}
              onClick={() => onChangeInterval(tf.id)}
            >
              {tf.label}
            </button>
          ))}
        </div>

        <div className="groww-chart-meta">
          <span className="groww-chart-tag">OHLCV Candlesticks + Volume</span>
        </div>
      </div>
    </div>
  );
};
