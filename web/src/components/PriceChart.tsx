import React, { useEffect, useRef } from 'react';
import { createChart, ColorType, type IChartApi, type ISeriesApi, type UTCTimestamp } from 'lightweight-charts';
import type { Candle } from '../types';

export type CandleInterval = '1m' | '5m' | '15m' | '1h';

interface PriceChartProps {
  symbol: string;
  interval: CandleInterval;
  onChangeInterval: (interval: CandleInterval) => void;
  candles: Candle[];
  liveCandle?: Candle;
}

const INTERVALS: CandleInterval[] = ['1m', '5m', '15m', '1h'];

export const PriceChart: React.FC<PriceChartProps> = ({
  symbol,
  interval,
  onChangeInterval,
  candles,
  liveCandle,
}) => {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const seriesRef = useRef<ISeriesApi<'Candlestick'> | null>(null);
  const volSeriesRef = useRef<ISeriesApi<'Histogram'> | null>(null);

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    const chart = createChart(el, {
      layout: {
        background: { type: ColorType.Solid, color: '#0d131f' },
        textColor: '#94a3b8',
      },
      grid: {
        vertLines: { color: '#1e293b' },
        horzLines: { color: '#1e293b' },
      },
      timeScale: {
        timeVisible: true,
        secondsVisible: false,
        borderColor: '#1e293b',
      },
      rightPriceScale: {
        borderColor: '#1e293b',
      },
      width: el.clientWidth || 640,
      height: 360,
    });

    const candleSeries = chart.addCandlestickSeries({
      upColor: '#10b981',
      downColor: '#ef4444',
      borderVisible: false,
      wickUpColor: '#10b981',
      wickDownColor: '#ef4444',
    });

    candleSeries.priceScale().applyOptions({
      scaleMargins: {
        top: 0.08,
        bottom: 0.25,
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
  }, []);

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
      color: c.close >= c.open ? 'rgba(16, 185, 129, 0.35)' : 'rgba(239, 68, 68, 0.35)',
    }));
    seriesRef.current.setData(formattedCandles);
    volSeriesRef.current.setData(formattedVol);
    chartRef.current?.timeScale().fitContent();
  }, [symbol, interval, candles]);

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
          ? 'rgba(16, 185, 129, 0.35)'
          : 'rgba(239, 68, 68, 0.35)',
    });
  }, [symbol, interval, liveCandle]);

  return (
    <div className="panel chart-panel">
      <div className="panel-header">
        <span>
          {symbol} · {interval} Candlesticks + Volume
        </span>
        <div className="timeframe-group" style={{ display: 'flex', gap: '4px' }}>
          {INTERVALS.map((tf) => (
            <button
              key={tf}
              type="button"
              className={`tab-btn ${interval === tf ? 'active' : ''}`}
              onClick={() => onChangeInterval(tf)}
              style={{ padding: '2px 8px', fontSize: '12px' }}
            >
              {tf}
            </button>
          ))}
        </div>
      </div>
      <div ref={containerRef} className="chart-canvas" />
    </div>
  );
};
